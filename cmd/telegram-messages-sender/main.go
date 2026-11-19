package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/dispatch"
	"telegram-messages-sender/internal/platform/keychain"
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

	engine := dispatch.New(state)
	client := telegram.NewClient(keychain.New())
	dispatchContext, stopDispatch := context.WithCancel(ctx)
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		err := client.Run(dispatchContext, func(live context.Context, connection *telegram.Connection) error {
			if _, err := state.BindAccount(live, connection.Status().AccountID); err != nil {
				return err
			}
			return engine.Run(live, connection)
		})
		if err != nil && dispatchContext.Err() == nil {
			engine.SetUnavailable(err)
		}
	}()

	err = owner.Serve(ctx, engine.Handler())
	stopDispatch()
	<-dispatchDone
	return err
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
