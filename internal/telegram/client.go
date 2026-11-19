package telegram

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"

	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/platform/keychain"
)

type SecureStore interface {
	session.Storage
	LoadCredentials(ctx context.Context) (id int, hash string, err error)
}

var errInvalidStoredSession = errors.New("invalid stored Telegram session")

type Client struct {
	store        SecureStore
	boundAccount func(context.Context) (domain.AccountID, error)
	newTransport func(int, string, gotd.Options) *gotd.Client
	running      atomic.Bool
}

func NewClient(store SecureStore, boundAccount func(context.Context) (domain.AccountID, error)) *Client {
	return &Client{store: store, boundAccount: boundAccount, newTransport: gotd.NewClient}
}

const SustainedInterruption = 5 * time.Second

type Status struct {
	AccountID          domain.AccountID
	DisplayName        string
	TransportAvailable bool
	Outage             uint64
}

type Connection struct {
	api         *tg.Client
	sendPeers   sync.Map
	ctx         context.Context
	account     domain.AccountID
	displayName string
	fileDC      func(context.Context, int) (tg.Invoker, error)
	ready       *atomic.Bool
	generation  *atomic.Uint64
	outage      *atomic.Uint64
	session     *stagedSession
}

func (c *Connection) Status() Status {
	status := Status{AccountID: c.account, DisplayName: c.displayName, TransportAvailable: c.ctx.Err() == nil && c.ready.Load() && c.session.healthy()}
	if c.outage != nil {
		status.Outage = c.outage.Load()
	}
	return status
}

// Run checks a saved personal session and runs use for the lifetime of the connection.
func (c *Client) Run(ctx context.Context, use func(context.Context, *Connection) error) error {
	_, err := c.start(ctx, nil, use)
	return err
}

// SignIn reuses an authorized session or starts an explicit, context-bound QR flow.
func (c *Client) SignIn(ctx context.Context, prompts SignInPrompts) (domain.AccountID, error) {
	return c.start(ctx, &prompts, nil)
}

func (c *Client) start(ctx context.Context, prompts *SignInPrompts, use func(context.Context, *Connection) error) (domain.AccountID, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !c.running.CompareAndSwap(false, true) {
		return 0, ErrClientBusy
	}
	defer c.running.Store(false)

	id, hash, err := c.store.LoadCredentials(ctx)
	if err != nil {
		if errors.Is(err, keychain.ErrMissing) {
			return 0, ErrCredentialsMissing
		}
		return 0, secretStoreError(ctx, err)
	}
	if id <= 0 || id > math.MaxInt32 || hash == "" {
		return 0, keychain.ErrInvalid
	}

	data, err := c.store.LoadSession(ctx)
	switch {
	case errors.Is(err, session.ErrNotFound):
		if prompts == nil {
			return 0, ErrSignInRequired
		}
	case errors.Is(err, keychain.ErrInvalid), err == nil && len(data) == 0:
		if prompts == nil {
			return 0, secretStoreError(ctx, keychain.ErrInvalid)
		}
	case err != nil:
		return 0, secretStoreError(ctx, err)
	default:
		defer clear(data)
		account, err := c.run(ctx, id, hash, data, nil, use)
		if errors.Is(err, errInvalidStoredSession) {
			if prompts == nil {
				return 0, secretStoreError(ctx, keychain.ErrInvalid)
			}
		} else if prompts == nil || (!errors.Is(err, ErrSessionRevoked) && !errors.Is(err, ErrAccountMismatch)) {
			return account, err
		}
	}

	return c.run(ctx, id, hash, nil, prompts, use)
}

func (c *Client) run(ctx context.Context, appID int, hash string, initial []byte, prompts *SignInPrompts, use func(context.Context, *Connection) error) (domain.AccountID, error) {
	saved := &stagedSession{store: c.store, data: bytes.Clone(initial)}
	defer saved.clear()
	if len(initial) != 0 {
		loader := session.Loader{Storage: saved}
		data, err := loader.Load(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, errInvalidStoredSession
		}
		var key crypto.Key
		copy(key[:], data.AuthKey)
		keyID := key.ID()
		if len(data.AuthKey) != len(key) || !bytes.Equal(data.AuthKeyID, keyID[:]) || key.Zero() {
			return 0, errInvalidStoredSession
		}
		saved.dc = data.DC
	}
	var ready atomic.Bool
	var generation, outage atomic.Uint64
	var readinessMu sync.Mutex
	var offlineAt time.Time
	dispatcher := tg.NewUpdateDispatcher()
	loggedIn := qrlogin.OnLoginToken(&dispatcher)
	options := gotd.Options{
		Resolver:       publicationResolver{Resolver: dcs.DefaultResolver()},
		SessionStorage: saved,
		UpdateHandler:  dispatcher,
		OnConnectionState: func(state gotd.ConnectionState) {
			readinessMu.Lock()
			defer readinessMu.Unlock()
			wasReady := ready.Load()
			isReady := state == gotd.ConnectionStateReady
			if wasReady && !isReady {
				offlineAt = time.Now()
			}
			if isReady && !offlineAt.IsZero() {
				if time.Since(offlineAt) >= SustainedInterruption {
					outage.Add(1)
				}
				offlineAt = time.Time{}
			}
			generation.Add(1)
			ready.Store(isReady)
		},
	}
	// A subscription probe can return a pre-login 401 after QR succeeds.
	// A fresh sign-in verifies authorization with its own post-login Self call.
	if prompts == nil {
		options.OnAuthLost = func(context.Context, error) error {
			ready.Store(false)
			return ErrSessionRevoked
		}
	}
	transport := c.newTransport(appID, hash, options)

	var account domain.AccountID
	var callbackErr, bindingErr error
	err := transport.Run(ctx, func(live context.Context) (err error) {
		defer func() { callbackErr = err }()
		if prompts != nil {
			err := runQR(live, transport.API(), appID, hash, transport.MigrateTo, nil, loggedIn, *prompts)
			if err != nil {
				return err
			}
		}
		self, err := personalAccount(live, transport.Self)
		if err != nil {
			return err
		}
		verifiedAccount := domain.AccountID(self.ID)
		bound, err := c.boundAccount(live)
		if err != nil {
			bindingErr = err
			return err
		}
		if bound != 0 && verifiedAccount != bound {
			bindingErr = ErrAccountMismatch
			return bindingErr
		}
		if err := saved.commit(live, initial, transport.Config().ThisDC); err != nil {
			return err
		}

		account = verifiedAccount
		if use != nil {
			name := strings.TrimSpace(self.FirstName + " " + self.LastName)
			if name == "" && self.Username != "" {
				name = "@" + self.Username
			}
			conn := &Connection{
				api: transport.API(), ctx: live, account: account, displayName: name, fileDC: transport.FileDC,
				ready: &ready, generation: &generation, outage: &outage, session: saved,
			}
			return use(live, conn)
		}
		return nil
	})
	ready.Store(false)
	switch {
	case ctx.Err() != nil, errors.Is(err, ErrSessionRevoked), gotd.IsAuthLost(err):
		return 0, connectionError(ctx, err)
	case bindingErr != nil:
		return 0, bindingErr
	case account != 0 && callbackErr != nil:
		return account, callbackErr
	case err != nil:
		return 0, connectionError(ctx, err)
	case callbackErr != nil:
		// gotd suppresses context.Canceled even when the login view returned it.
		return 0, connectionError(ctx, callbackErr)
	default:
		return account, nil
	}
}

type stagedSession struct {
	mu        sync.Mutex
	store     session.Storage
	data      []byte
	dc        int
	changed   chan struct{}
	committed bool
	available atomic.Bool
}

func (s *stagedSession) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.data) == 0 {
		return nil, session.ErrNotFound
	}
	return bytes.Clone(s.data), nil
}

func (s *stagedSession) StoreSession(ctx context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if s.committed {
		s.available.Store(false)
		if err := s.store.StoreSession(ctx, bytes.Clone(data)); err != nil {
			return secretStoreError(ctx, err)
		}
	} else {
		var memory session.StorageMemory
		if err := memory.StoreSession(ctx, data); err != nil {
			return ErrSessionUnavailable
		}
		loader := session.Loader{Storage: &memory}
		staged, err := loader.Load(ctx)
		if err != nil {
			return ErrSessionUnavailable
		}
		s.dc = staged.DC
	}
	clear(s.data)
	s.data = bytes.Clone(data)
	s.available.Store(s.committed)
	if s.changed != nil {
		close(s.changed)
		s.changed = nil
	}
	return nil
}

func (s *stagedSession) commit(ctx context.Context, initial []byte, primaryDC int) error {
	if primaryDC <= 0 {
		return ErrSessionUnavailable
	}
	// gotd can finish RPCs before writing the primary key, especially after a DC change.
	for {
		changed, err := s.commitWhenCurrent(ctx, initial, primaryDC)
		if changed == nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (s *stagedSession) commitWhenCurrent(ctx context.Context, initial []byte, primaryDC int) (<-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.dc != primaryDC {
		if s.changed == nil {
			s.changed = make(chan struct{})
		}
		return s.changed, nil
	}
	if !bytes.Equal(s.data, initial) {
		s.available.Store(false)
		if err := s.store.StoreSession(ctx, bytes.Clone(s.data)); err != nil {
			return nil, secretStoreError(ctx, err)
		}
	}
	s.committed = true
	s.available.Store(true)
	return nil, nil
}

func (s *stagedSession) healthy() bool {
	// Dispatch can read readiness while holding its submission gate.
	return s.available.Load()
}

func (s *stagedSession) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.data)
}
