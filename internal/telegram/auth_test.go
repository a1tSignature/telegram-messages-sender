package telegram

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/neo"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgtest"
	"github.com/gotd/td/tgtest/cluster"

	"telegram-messages-sender/internal/platform/keychain"
)

type rpcFixture func(input bin.Encoder) (bin.Encoder, error)

func (f rpcFixture) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	result, err := f(input)
	if err != nil {
		return err
	}
	var buf bin.Buffer
	if err := result.Encode(&buf); err != nil {
		return err
	}
	return output.Decode(&buf)
}

func TestQRRefreshCancellationAndPassword(t *testing.T) {
	const appHash = "fixture-app-hash"
	t.Run("expiry refresh and cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		clock := neo.NewTime(time.Unix(1700000000, 0))
		var exports int
		api := tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
			req, ok := input.(*tg.AuthExportLoginTokenRequest)
			if !ok || req.APIID != 1234 || req.APIHash != appHash {
				t.Fatalf("unexpected QR login request %T", input)
			}
			exports++
			return &tg.AuthLoginToken{Token: []byte{byte(exports)}, Expires: int(clock.Now().Add(time.Second).Unix())}, nil
		}))

		var displayed []string
		err := runQR(ctx, api, 1234, appHash, nil, clock, nil, SignInPrompts{
			ShowQR: func(_ context.Context, url string, expires time.Time) error {
				if !expires.After(clock.Now()) || !strings.HasPrefix(url, "tg://login?token=") {
					t.Fatal("invalid code or expiry")
				}
				displayed = append(displayed, url)
				if len(displayed) == 1 {
					clock.Travel(time.Second)
				} else {
					cancel()
				}
				return nil
			},
			Password: func(context.Context) (string, error) {
				t.Fatal("password requested before Telegram asked for it")
				return "", nil
			},
		})
		if !errors.Is(err, context.Canceled) || exports != 2 || len(displayed) != 2 || displayed[0] == displayed[1] {
			t.Fatalf("expiry/cancellation mismatch, got %d exports, %d displays, error %v", exports, len(displayed), err)
		}
	})

	t.Run("two-step verification", func(t *testing.T) {
		password := passwordFixture(t)
		var prompted, checked bool
		api := tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
			switch req := input.(type) {
			case *tg.AuthExportLoginTokenRequest:
				return nil, tgerr.New(401, "SESSION_PASSWORD_NEEDED")
			case *tg.AccountGetPasswordRequest:
				if !prompted {
					t.Fatal("password parameters requested before prompting")
				}
				return password, nil
			case *tg.AuthCheckPasswordRequest:
				proof, ok := req.Password.(*tg.InputCheckPasswordSRP)
				if !ok || proof.SRPID != password.SRPID || len(proof.A) == 0 || len(proof.M1) != 32 {
					t.Fatal("invalid two-step proof")
				}
				checked = true
				return &tg.AuthAuthorization{User: &tg.User{ID: 42}}, nil
			default:
				t.Fatalf("unexpected password flow request %T", input)
				return nil, nil
			}
		}))
		err := runQR(t.Context(), api, 1234, appHash, nil, nil, nil, SignInPrompts{
			ShowQR: func(context.Context, string, time.Time) error { return nil },
			Password: func(context.Context) (string, error) {
				prompted = true
				return "fixture-password", nil
			},
		})
		if err != nil || !checked {
			t.Fatalf("two-step verification returned %v", err)
		}
	})

	t.Run("sensitive errors are sanitized", func(t *testing.T) {
		api := tg.NewClient(rpcFixture(func(input bin.Encoder) (bin.Encoder, error) {
			return &tg.AuthLoginToken{Token: []byte("fixture-token"), Expires: int(time.Now().Add(time.Minute).Unix())}, nil
		}))
		err := runQR(t.Context(), api, 1234, appHash, nil, nil, nil, SignInPrompts{
			ShowQR: func(_ context.Context, url string, _ time.Time) error { return errors.New(url + appHash) },
		})
		if err != ErrQRDisplay {
			t.Fatal("QR display error included the callback cause")
		}
	})
}

func passwordFixture(t *testing.T) *tg.AccountPassword {
	t.Helper()
	p, err := (exchange.TestServerRNG{}).DhPrime()
	if err != nil {
		t.Fatal(err)
	}
	algo := &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
		Salt1: []byte("fixture-salt-one"), Salt2: []byte("fixture-salt-two"), G: 3, P: p.Bytes(),
	}
	b := new(big.Int).Exp(big.NewInt(3), big.NewInt(130001), p)

	password := &tg.AccountPassword{
		HasPassword: true, SRPID: 11, SRPB: b.Bytes(),
		CurrentAlgo: algo, NewAlgo: algo, NewSecureAlgo: &tg.SecurePasswordKdfAlgoUnknown{},
	}
	return password
}

type localSecureStore struct {
	session.StorageMemory
	loadErr  error
	writeErr error
}

func (*localSecureStore) LoadCredentials(context.Context) (int, string, error) {
	return 1234, "fixture-app-hash", nil
}

func (s *localSecureStore) LoadSession(ctx context.Context) ([]byte, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.StorageMemory.LoadSession(ctx)
}

func (s *localSecureStore) StoreSession(ctx context.Context, data []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.StorageMemory.StoreSession(ctx, data)
}

func TestDedicatedSessionOverLocalMTProto(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	network := cluster.NewCluster(cluster.Options{})
	dispatch := network.Dispatch(2, "local Telegram fixture")
	var accepted atomic.Pointer[[8]byte]
	var logins atomic.Int32
	var presentQR atomic.Bool
	type delayedProbe struct {
		captured chan struct{}
		observed chan struct{}
		request  *tgtest.Request
	}
	var stale atomic.Pointer[delayedProbe]
	user := &tg.User{ID: 42, Self: true}
	dispatch.HandleFunc(tg.AuthExportLoginTokenRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		logins.Add(1)
		if presentQR.Load() {
			return s.SendResult(req, &tg.AuthLoginToken{Token: []byte("fixture-token"), Expires: int(time.Now().Add(time.Minute).Unix())})
		}
		key := [8]byte(req.Session.AuthKey.ID)
		accepted.Store(&key)
		return s.SendResult(req, &tg.AuthLoginTokenSuccess{Authorization: &tg.AuthAuthorization{User: user}})
	})
	dispatch.HandleFunc(tg.UsersGetUsersRequestTypeID, func(s *tgtest.Server, req *tgtest.Request) error {
		unauthorized := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
		probe := stale.Load()
		if key := accepted.Load(); key != nil && *key == [8]byte(req.Session.AuthKey.ID) {
			if probe != nil {
				if err := s.SendErr(probe.request, unauthorized); err != nil {
					return err
				}
				select {
				case <-probe.observed:
				case <-req.RequestCtx.Done():
					return req.RequestCtx.Err()
				}
			}
			return s.SendVector(req, user)
		}
		if probe != nil {
			probe.request = req
			close(probe.captured)
			return nil
		}
		return s.SendErr(req, unauthorized)
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- network.Up(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-serverDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("local Telegram transport: %v", err)
		}
	})
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-network.Ready():
	}

	storage := &localSecureStore{}
	newClient := func() *Client {
		client := NewClient(storage)
		client.newTransport = func(id int, hash string, opts gotd.Options) *gotd.Client {
			opts.PublicKeys = network.Keys()
			opts.Resolver = network.Resolver()
			opts.DCList = network.List()
			if probe := stale.Load(); probe != nil {
				opts.OnSelfError = func(context.Context, error) error {
					close(probe.observed)
					return nil
				}
				opts.Middlewares = []gotd.Middleware{gotd.MiddlewareFunc(func(next tg.Invoker) gotd.InvokeFunc {
					return func(ctx context.Context, request bin.Encoder, response bin.Decoder) error {
						if _, qr := request.(*tg.AuthExportLoginTokenRequest); qr {
							select {
							case <-probe.captured:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
						return next.Invoke(ctx, request, response)
					}
				})}
			}
			return gotd.NewClient(id, hash, opts)
		}
		return client
	}

	const expectedID = 42
	var lastConnection *Connection
	check := func(ctx context.Context, conn *Connection) error {
		lastConnection = conn
		if status := conn.Status(); status.AccountID != expectedID {
			t.Errorf("unexpected authorized account %d", status.AccountID)
		}
		ticks := time.NewTicker(time.Millisecond)
		defer ticks.Stop()
		for !conn.Status().TransportAvailable {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticks.C:
			}
		}
		return nil
	}
	prompts := SignInPrompts{ShowQR: func(context.Context, string, time.Time) error { return errors.New("unexpected QR") }}
	if err := newClient().Run(ctx, check); err != ErrSignInRequired || logins.Load() != 0 {
		t.Fatalf("passive startup requested credentials or connected for QR: %v", err)
	}

	presentQR.Store(true)
	cancelled, stop := context.WithCancel(ctx)
	_, err := newClient().SignIn(cancelled, SignInPrompts{ShowQR: func(context.Context, string, time.Time) error {
		stop()
		return nil
	}})
	if err != context.Canceled {
		t.Fatalf("cancelled QR result: %v", err)
	}
	if id, err := newClient().SignIn(ctx, SignInPrompts{ShowQR: func(context.Context, string, time.Time) error {
		return context.Canceled
	}}); err != context.Canceled || id != 0 {
		t.Fatalf("QR cancelled by the view returned account %d, %v", id, err)
	}
	if _, err := storage.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("a cancelled login persisted a session")
	}
	presentQR.Store(false)

	storage.writeErr = keychain.ErrDenied
	stale.Store(&delayedProbe{captured: make(chan struct{}), observed: make(chan struct{})})
	if id, err := newClient().SignIn(ctx, prompts); err != keychain.ErrDenied || id != 0 {
		t.Fatalf("authorization escaped a failed Keychain commit: account %d, %v", id, err)
	}
	stale.Store(nil)
	storage.writeErr = nil

	if id, err := newClient().SignIn(ctx, prompts); err != nil || id != expectedID {
		t.Fatalf("first sign-in account %d, %v", id, err)
	}
	before := logins.Load()
	if err := newClient().Run(ctx, check); err != nil || logins.Load() != before {
		t.Fatalf("did not reuse the saved authorization: %v", err)
	}
	if lastConnection == nil || lastConnection.Status().TransportAvailable {
		t.Fatal("transport reported available after the connection ended")
	}

	storage.loadErr = keychain.ErrDenied
	if err := newClient().Run(ctx, check); err != keychain.ErrDenied || logins.Load() != before {
		t.Fatalf("access denial was mistaken for a missing session: %v", err)
	}
	storage.loadErr = nil

	accepted.Store(nil)
	if err := newClient().Run(ctx, check); err != ErrSessionRevoked || logins.Load() != before {
		t.Fatalf("revoked authorization was not surfaced: %v", err)
	}
	if id, err := newClient().SignIn(ctx, prompts); err != nil || id != expectedID || logins.Load() != before+1 {
		t.Fatalf("explicit request did not replace the revoked authorization: account %d, %v", id, err)
	}
}

type stagingWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *stagingWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCommitWaitsForCurrentPrimarySession(t *testing.T) {
	for _, tt := range []struct {
		name      string
		staleDC   bool
		cancelled bool
	}{
		{name: "initial key arrives after authorization"},
		{name: "old data center key after migration", staleDC: true},
		{name: "cancel before primary key arrives", staleDC: true, cancelled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			observed := &stagingWaitContext{Context: ctx, waiting: make(chan struct{})}
			durable := &session.StorageMemory{}
			staged := &stagedSession{store: durable}
			staging := session.Loader{Storage: staged}
			const primaryDC = 3
			if tt.staleDC {
				if err := staging.Save(ctx, &session.Data{DC: 2}); err != nil {
					t.Fatal(err)
				}
			}

			result := make(chan error, 1)
			go func() { result <- staged.commit(observed, nil, primaryDC) }()
			select {
			case <-observed.waiting:
			case err := <-result:
				t.Fatalf("commit finished before staging the primary session: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			if tt.cancelled {
				cancel()
				if err := <-result; err != context.Canceled {
					t.Fatalf("commit wait returned %v after cancellation", err)
				}
				if _, err := durable.LoadSession(t.Context()); !errors.Is(err, session.ErrNotFound) {
					t.Fatal("cancelled commit persisted a session")
				}
				return
			}

			if err := staging.Save(ctx, &session.Data{DC: primaryDC}); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			storage := session.Loader{Storage: durable}
			saved, err := storage.Load(ctx)
			if err != nil || saved.DC != primaryDC {
				t.Fatalf("commit did not persist the current primary session, error %v", err)
			}
		})
	}
}

type blockingSessionStore struct {
	session.StorageMemory
	entered chan struct{}
	release chan struct{}
	err     error
}

func (s *blockingSessionStore) StoreSession(ctx context.Context, data []byte) error {
	close(s.entered)
	<-s.release
	if s.err != nil {
		return s.err
	}
	return s.StorageMemory.StoreSession(ctx, data)
}

func TestSessionHealthDoesNotWaitForPersistence(t *testing.T) {
	for _, writeErr := range []error{nil, keychain.ErrDenied} {
		name := "successful write"
		if writeErr != nil {
			name = "failed write"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			store := &blockingSessionStore{entered: make(chan struct{}), release: make(chan struct{}), err: writeErr}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(store.release) }) }
			defer release()
			saved := &stagedSession{store: store, dc: 2}
			if saved.healthy() {
				t.Fatal("uncommitted session reported ready")
			}
			if err := saved.commit(ctx, nil, 2); err != nil || !saved.healthy() {
				t.Fatalf("authorized durable session was unavailable: %v", err)
			}
			stored := make(chan error, 1)
			go func() { stored <- saved.StoreSession(ctx, []byte("updated session")) }()
			select {
			case <-store.entered:
			case <-ctx.Done():
				t.Fatal("session storage did not start")
			}
			status := make(chan bool, 1)
			go func() { status <- saved.healthy() }()
			select {
			case healthy := <-status:
				if healthy {
					t.Fatal("session reported ready during persistence")
				}
			case <-ctx.Done():
				t.Fatal("session readiness waited for persistence")
			}
			release()
			if err := <-stored; !errors.Is(err, writeErr) {
				t.Fatalf("session storage returned %v, want %v", err, writeErr)
			}
			if got := saved.healthy(); got != (writeErr == nil) {
				t.Fatalf("session readiness after persistence = %v, storage error = %v", got, writeErr)
			}
		})
	}
}
