package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"

	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/desktop"
	"telegram-messages-sender/internal/dispatch"
	"telegram-messages-sender/internal/platform/keychain"
	"telegram-messages-sender/internal/platform/wake"
	"telegram-messages-sender/internal/store"
	"telegram-messages-sender/internal/telegram"
)

func run(ctx context.Context) error {
	dir, err := control.StateDir()
	if err != nil {
		return err
	}
	owner, err := control.Acquire(dir)
	if err != nil {
		return err
	}
	defer owner.Close()

	state, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		return err
	}
	defer state.Close()

	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	engine := dispatch.New(state)
	keys := keychain.New()
	session := &telegramOwner{client: telegram.NewClient(keys, state.BoundAccount), reconnect: make(chan struct{}, 1)}
	stopObserving := wake.Observe(engine.NotifyWake)
	defer stopObserving()

	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		session.run(ctx, engine, state)
	}()
	controlDone := make(chan error, 1)
	go func() {
		err := owner.Serve(ctx, engine.Handler())
		controlDone <- err
		shutdown()
	}()

	application := fyneapp.NewWithID("telegram-messages-sender")
	application.Lifecycle().SetOnStopped(shutdown)
	window := desktop.NewWindow(ctx, application, desktop.Backend{
		Command: engine.Handler(), Snapshot: state.Snapshot, Save: engine.SavePublication,
		SetAllowNonAdmin: engine.SetAllowNonAdmin,
		ListSaved:        session.savedMessages, Destinations: session.destinations, SignIn: session.signIn,
		SignOut:     func(ctx context.Context, localOnly bool) error { return session.signOut(ctx, engine, localOnly) },
		Credentials: keys.StoreCredentials,
	}, shutdown)
	go func() {
		<-ctx.Done()
		fyne.Do(application.Quit)
	}()
	window.Watch()
	window.ShowAndRun()
	shutdown()

	err = <-controlDone
	select {
	case <-dispatchDone:
	case <-time.After(7 * time.Second):
		if err == nil {
			err = errors.New("Telegram connection did not stop before the shutdown deadline")
		}
	}
	return err
}

type telegramOwner struct {
	client    *telegram.Client
	active    atomic.Pointer[telegram.Connection]
	reconnect chan struct{}

	mu             sync.Mutex
	stopConnection context.CancelFunc
	connectionDone <-chan struct{}
}

func (s *telegramOwner) run(ctx context.Context, engine *dispatch.Engine, state *store.Store) {
	for ctx.Err() == nil {
		err := s.connect(ctx, engine, state)
		if ctx.Err() != nil {
			return
		}
		var retry <-chan time.Time
		var timer *time.Timer
		if err != nil {
			engine.SetUnavailable(err)
			if errors.Is(err, telegram.ErrTransport) || errors.Is(err, telegram.ErrClientBusy) {
				timer = time.NewTimer(telegram.SustainedInterruption)
				retry = timer.C
			}
		}
		select {
		case <-ctx.Done():
		case <-s.reconnect:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *telegramOwner) connect(ctx context.Context, engine *dispatch.Engine, state *store.Store) error {
	s.mu.Lock()
	ctx, stopConnection := context.WithCancel(ctx)
	connectionDone := make(chan struct{})
	s.stopConnection, s.connectionDone = stopConnection, connectionDone
	s.mu.Unlock()
	defer close(connectionDone)
	defer stopConnection()
	if ctx.Err() != nil {
		return nil
	}

	// Keep the Telegram connection live while the dispatcher drains an already submitted request.
	network, disconnect := context.WithCancel(context.WithoutCancel(ctx))
	defer disconnect()
	stop := context.AfterFunc(ctx, func() {
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-connectionDone:
		case <-timer.C:
			disconnect()
		}
	})
	defer stop()
	err := s.client.Run(network, func(live context.Context, connection *telegram.Connection) error {
		dispatchCtx, stopDispatch := context.WithCancel(ctx)
		stopOnDisconnect := context.AfterFunc(live, stopDispatch)
		defer stopOnDisconnect()
		defer stopDispatch()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := state.BindAccount(dispatchCtx, connection.Status().AccountID); err != nil {
			return err
		}
		s.active.Store(connection)
		defer s.active.Store(nil)
		return engine.RunRecovering(dispatchCtx, connection)
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *telegramOwner) signIn(ctx context.Context, prompts telegram.SignInPrompts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.client.SignIn(ctx, prompts); err != nil {
		return err
	}
	s.requestReconnect()
	return nil
}

func (s *telegramOwner) signOut(ctx context.Context, engine *dispatch.Engine, localOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	defer s.requestReconnect()
	if s.stopConnection != nil {
		s.stopConnection()
		select {
		case <-s.connectionDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := s.client.SignOut(ctx, localOnly); err != nil {
		return err
	}
	if err := engine.ResetAccount(ctx); err != nil {
		return fmt.Errorf("could not delete local account data. Choose Sign out again to retry: %w", err)
	}
	return nil
}

func (s *telegramOwner) requestReconnect() {
	select {
	case s.reconnect <- struct{}{}:
	default:
	}
}

func (s *telegramOwner) savedMessages(ctx context.Context, offset, limit int) (telegram.SavedPage, error) {
	if connection := s.active.Load(); connection != nil {
		return connection.ListSaved(ctx, offset, limit)
	}
	return telegram.SavedPage{}, telegram.ErrSignInRequired
}

func (s *telegramOwner) destinations(ctx context.Context) ([]telegram.Destination, error) {
	if connection := s.active.Load(); connection != nil {
		return connection.Destinations(ctx)
	}
	return nil, telegram.ErrSignInRequired
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if !errors.Is(err, control.ErrOwnerRunning) {
			os.Exit(1)
		}
	}
}
