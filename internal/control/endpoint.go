package control

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const endpointFile = "endpoint.json"

var (
	ErrAppClosed = errors.New("the desktop app is closed")
	errEndpoint  = errors.New("invalid desktop control endpoint")
)

type endpoint struct {
	PID         int    `json:"pid"`
	StartedAtNS int64  `json:"started_at_ns"`
	Port        int    `json:"port"`
	Token       string `json:"token"`
}

func StateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "telegram-messages-sender"), nil
}

func checkStateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByUser(info) {
		return errors.New("desktop state directory must be owned by the current user with mode 0700")
	}
	return nil
}

func checkPrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByUser(info) {
		return errors.New("desktop control file must be owned by the current user with mode 0600")
	}
	return nil
}

func publishEndpoint(dir string, value endpoint) error {
	file, err := os.CreateTemp(dir, ".endpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(dir, endpointFile))
}

func readEndpoint(dir string) (endpoint, error) {
	if err := checkStateDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return endpoint{}, ErrAppClosed
		}
		return endpoint{}, err
	}
	file, err := os.OpenFile(filepath.Join(dir, endpointFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return endpoint{}, ErrAppClosed
	}
	if err != nil {
		return endpoint{}, err
	}
	defer file.Close()
	if err := checkPrivateFile(file); err != nil {
		return endpoint{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return endpoint{}, err
	}
	var value endpoint
	if len(data) > 1024 || json.Unmarshal(data, &value) != nil {
		return endpoint{}, errEndpoint
	}
	token, err := hex.DecodeString(value.Token)
	if err != nil || len(token) != 32 || value.PID <= 0 || value.Port < 1 || value.Port > 65535 {
		return endpoint{}, errEndpoint
	}
	return value, nil
}
