package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"telegram-messages-sender/internal/app"
)

const (
	protocolVersion = 1
	commandPath     = "/command"
	loopback        = "127.0.0.1"
	maxRequestBytes = 4096
	commandTimeout  = 5 * time.Second
)

type request struct {
	Version int         `json:"version"`
	Command app.Command `json:"command"`
}

type response struct {
	Version int        `json:"version"`
	Result  app.Result `json:"result"`
}

func (o *Owner) Serve(ctx context.Context, handler app.Handler) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort(loopback, "0"))
	if err != nil {
		return err
	}
	defer listener.Close()

	pid := os.Getpid()
	startedAt, err := processStartTime(pid)
	if err != nil {
		return fmt.Errorf("read desktop process start time: %w", err)
	}
	var secret [32]byte
	rand.Read(secret[:])
	value := endpoint{
		PID:         pid,
		StartedAtNS: startedAt,
		Port:        listener.Addr().(*net.TCPAddr).Port,
		Token:       hex.EncodeToString(secret[:]),
	}
	if err := publishEndpoint(o.dir, value); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(o.dir, endpointFile))

	server := &http.Server{
		Handler:        commandHandler(value.Token, handler),
		ReadTimeout:    commandTimeout,
		WriteTimeout:   2 * commandTimeout,
		IdleTimeout:    commandTimeout,
		MaxHeaderBytes: maxRequestBytes,
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	select {
	case err := <-serveErr:
		server.Close()
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), commandTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}

func commandHandler(token string, handler app.Handler) http.Handler {
	auth := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), auth) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != commandPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input request
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Version != protocolVersion {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), commandTimeout)
		defer cancel()
		result, err := handler(ctx, input.Command)
		switch {
		case errors.Is(err, app.ErrInvalidCommand):
			w.WriteHeader(http.StatusBadRequest)
		case errors.Is(err, app.ErrUnknownPublication):
			w.WriteHeader(http.StatusNotFound)
		case errors.Is(err, app.ErrVerificationRequired):
			w.WriteHeader(http.StatusConflict)
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response{Version: protocolVersion, Result: result})
		}
	})
}

var client = &http.Client{
	Timeout: commandTimeout,
	Transport: &http.Transport{
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func Call(ctx context.Context, dir string, command app.Command) (app.Result, error) {
	value, err := readEndpoint(dir)
	if err != nil {
		return app.Result{}, err
	}
	startedAt, err := processStartTime(value.PID)
	if err != nil || startedAt == 0 || startedAt != value.StartedAtNS {
		return app.Result{}, ErrAppClosed
	}
	body, err := json.Marshal(request{Version: protocolVersion, Command: command})
	if err != nil {
		return app.Result{}, err
	}
	address := net.JoinHostPort(loopback, strconv.Itoa(value.Port))
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+commandPath, bytes.NewReader(body))
	if err != nil {
		return app.Result{}, err
	}
	r.Header.Set("Authorization", "Bearer "+value.Token)
	r.Header.Set("Content-Type", "application/json")

	reply, err := client.Do(r)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return app.Result{}, ErrAppClosed
	}
	if err != nil {
		return app.Result{}, err
	}
	defer reply.Body.Close()
	switch reply.StatusCode {
	case http.StatusOK:
		var output response
		if err := json.NewDecoder(reply.Body).Decode(&output); err != nil || output.Version != protocolVersion {
			return app.Result{}, errors.New("invalid response from desktop app")
		}
		return output.Result, nil
	case http.StatusBadRequest:
		return app.Result{}, app.ErrInvalidCommand
	case http.StatusNotFound:
		return app.Result{}, app.ErrUnknownPublication
	case http.StatusConflict:
		return app.Result{}, app.ErrVerificationRequired
	default:
		return app.Result{}, fmt.Errorf("desktop control request failed (HTTP %d)", reply.StatusCode)
	}
}
