package telegram

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/clock"
	"github.com/gotd/td/session"
	gotd "github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/platform/keychain"
)

var (
	ErrClientBusy         = errors.New("Telegram session already in use")
	ErrCredentialsMissing = errors.New("Telegram application credentials missing")
	ErrSignInRequired     = errors.New("Telegram sign-in required")
	ErrSessionRevoked     = errors.New("Telegram authorization revoked or expired")
	ErrSessionUnavailable = errors.New("Telegram session could not be stored or restored")
	ErrPersonalAccount    = errors.New("Telegram personal account required")
	ErrTransport          = errors.New("Telegram transport unavailable")
	ErrQRDisplay          = errors.New("Telegram QR code could not be displayed")
	ErrPasswordRequired   = errors.New("Telegram two-step password required")
	ErrPasswordInput      = errors.New("Telegram two-step password could not be read")
	ErrPasswordInvalid    = errors.New("Telegram two-step password rejected")
	ErrSignInFailed       = errors.New("Telegram QR sign-in failed")
)

type SignInPrompts struct {
	ShowQR   func(ctx context.Context, loginURL string, expires time.Time) error
	Password func(ctx context.Context) (string, error)
}

func runQR(
	ctx context.Context,
	api *tg.Client,
	appID int,
	hash string,
	migrate func(context.Context, int) error,
	clock clock.Clock,
	loggedIn qrlogin.LoggedIn,
	prompts SignInPrompts,
) error {
	if prompts.ShowQR == nil {
		return ErrQRDisplay
	}
	qr := qrlogin.NewQR(api, appID, hash, qrlogin.Options{
		Migrate: migrate,
		Clock:   clock,
		Password: func(ctx context.Context) (string, error) {
			if prompts.Password == nil {
				return "", ErrPasswordRequired
			}
			password, err := prompts.Password(ctx)
			switch {
			case ctx.Err() != nil:
				return "", ctx.Err()
			case errors.Is(err, context.Canceled):
				return "", context.Canceled
			case err != nil:
				return "", ErrPasswordInput
			case password == "":
				return "", ErrPasswordRequired
			default:
				return password, nil
			}
		},
	})

	_, err := qr.Auth(ctx, loggedIn, func(ctx context.Context, token qrlogin.Token) error {
		if err := prompts.ShowQR(ctx, token.URL(), token.Expires()); err != nil {
			if errors.Is(err, context.Canceled) {
				return context.Canceled
			}
			return ErrQRDisplay
		}
		return ctx.Err()
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err == nil:
		return nil
	case errors.Is(err, auth.ErrPasswordInvalid):
		return ErrPasswordInvalid
	case errors.Is(err, ErrQRDisplay), errors.Is(err, ErrPasswordInput), errors.Is(err, ErrPasswordRequired), errors.Is(err, context.Canceled):
		return connectionError(ctx, err)
	default:
		return ErrSignInFailed
	}
}

func personalAccount(ctx context.Context, self func(context.Context) (*tg.User, error)) (domain.AccountID, error) {
	user, err := self(ctx)
	switch {
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case gotd.IsAuthLost(err), tgerr.IsCode(err, 401):
		return 0, ErrSessionRevoked
	case err != nil:
		return 0, ErrTransport
	case user == nil || user.ID <= 0 || user.Bot:
		return 0, ErrPersonalAccount
	default:
		return domain.AccountID(user.ID), nil
	}
}

func connectionError(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	for _, known := range []error{
		ErrSessionRevoked, ErrSessionUnavailable, ErrPersonalAccount, ErrTransport,
		ErrQRDisplay, ErrPasswordRequired, ErrPasswordInput, ErrPasswordInvalid, ErrSignInFailed,
		keychain.ErrDenied, keychain.ErrInvalid, keychain.ErrUnavailable,
		context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	switch {
	case gotd.IsAuthLost(err), tgerr.IsCode(err, 401):
		return ErrSessionRevoked
	case errors.Is(err, session.ErrVersionUnsupported):
		return keychain.ErrInvalid
	default:
		return ErrTransport
	}
}

func secretStoreError(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	for _, known := range []error{keychain.ErrDenied, keychain.ErrInvalid, keychain.ErrUnavailable} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrSessionUnavailable
}
