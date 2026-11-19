package keychain

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>

static CFMutableDictionaryRef query(CFStringRef service, CFStringRef account) {
	CFMutableDictionaryRef item = CFDictionaryCreateMutable(
		NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(item, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(item, kSecAttrService, service);
	CFDictionarySetValue(item, kSecAttrAccount, account);
	return item;
}

static OSStatus load(CFStringRef service, CFStringRef account, CFDataRef *data) {
	CFMutableDictionaryRef item = query(service, account);
	CFDictionarySetValue(item, kSecReturnData, kCFBooleanTrue);
	CFDictionarySetValue(item, kSecMatchLimit, kSecMatchLimitOne);
	OSStatus status = SecItemCopyMatching(item, (CFTypeRef *)data);
	CFRelease(item);
	return status;
}

static OSStatus save(CFStringRef service, CFStringRef account, const UInt8 *bytes, CFIndex length) {
	CFMutableDictionaryRef item = query(service, account);
	CFDataRef data = CFDataCreate(NULL, bytes, length);
	const void *keys[] = {kSecValueData};
	const void *values[] = {data};
	CFDictionaryRef updated = CFDictionaryCreate(
		NULL, keys, values, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

	OSStatus status = SecItemUpdate(item, updated);
	if (status == errSecItemNotFound) {
		CFDictionarySetValue(item, kSecValueData, data);
		status = SecItemAdd(item, NULL);
		if (status == errSecDuplicateItem) {
			CFDictionaryRemoveValue(item, kSecValueData);
			status = SecItemUpdate(item, updated);
		}
	}

	CFRelease(updated);
	CFRelease(data);
	CFRelease(item);
	return status;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"unsafe"

	"github.com/gotd/td/session"
)

const (
	sessionEntry     = "telegram-session"
	credentialsEntry = "telegram-app-credentials"
)

type Store struct {
	service string
}

func New() Store {
	return Store{service: "telegram-messages-sender"}
}

var _ session.Storage = Store{}

func (s Store) LoadSession(ctx context.Context) ([]byte, error) {
	data, err := s.load(ctx, sessionEntry)
	if errors.Is(err, ErrMissing) {
		return nil, session.ErrNotFound
	}
	return data, err
}

func (s Store) StoreSession(ctx context.Context, data []byte) error {
	return s.save(ctx, sessionEntry, data)
}

type appCredentials struct {
	ID   int    `json:"id"`
	Hash string `json:"hash"`
}

func (s Store) LoadCredentials(ctx context.Context) (int, string, error) {
	data, err := s.load(ctx, credentialsEntry)
	if err != nil {
		return 0, "", err
	}
	defer clear(data)

	var creds appCredentials
	if err := json.Unmarshal(data, &creds); err != nil || !creds.valid() {
		return 0, "", ErrInvalid
	}
	return creds.ID, creds.Hash, nil
}

func (s Store) StoreCredentials(ctx context.Context, id int, hash string) error {
	creds := appCredentials{ID: id, Hash: hash}
	if !creds.valid() {
		return ErrInvalid
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return ErrInvalid
	}
	defer clear(data)
	return s.save(ctx, credentialsEntry, data)
}

func (c appCredentials) valid() bool {
	return c.ID > 0 && c.ID <= math.MaxInt32 && c.Hash != ""
}

func (s Store) load(ctx context.Context, entry string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	service, account := s.names(entry)
	defer C.CFRelease(C.CFTypeRef(service))
	defer C.CFRelease(C.CFTypeRef(account))

	var data C.CFDataRef
	if err := keychainError(C.load(service, account, &data)); err != nil {
		return nil, err
	}
	defer C.CFRelease(C.CFTypeRef(data))

	length := C.CFDataGetLength(data)
	if length <= 0 || length > math.MaxInt32 {
		return nil, ErrInvalid
	}
	return C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(length)), nil
}

func (s Store) save(ctx context.Context, entry string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return ErrInvalid
	}
	service, account := s.names(entry)
	defer C.CFRelease(C.CFTypeRef(service))
	defer C.CFRelease(C.CFTypeRef(account))

	return keychainError(C.save(service, account, (*C.UInt8)(unsafe.Pointer(&data[0])), C.CFIndex(len(data))))
}

func (s Store) names(entry string) (C.CFStringRef, C.CFStringRef) {
	return cfString(s.service), cfString(entry)
}

func cfString(value string) C.CFStringRef {
	return C.CFStringCreateWithBytes(
		0, (*C.UInt8)(unsafe.Pointer(unsafe.StringData(value))),
		C.CFIndex(len(value)), C.kCFStringEncodingUTF8, 0,
	)
}

func keychainError(status C.OSStatus) error {
	switch status {
	case C.errSecSuccess:
		return nil
	case C.errSecItemNotFound:
		return ErrMissing
	case C.errSecAuthFailed, C.errSecInteractionNotAllowed, C.errSecUserCanceled, C.errSecMissingEntitlement, C.errSecNoAccessForItem:
		return ErrDenied
	case C.errSecDecode:
		return ErrInvalid
	default:
		return ErrUnavailable
	}
}
