package keychain

import "errors"

var (
	ErrMissing     = errors.New("Keychain item missing")
	ErrDenied      = errors.New("Keychain access denied")
	ErrUnavailable = errors.New("Keychain unavailable")
	ErrInvalid     = errors.New("Keychain item invalid")
)
