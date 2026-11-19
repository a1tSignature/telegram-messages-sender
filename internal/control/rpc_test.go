package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

func TestOwnerRetainsEndpointAndRecoversAfterProcessExit(t *testing.T) {
	dir := privateStateDir(t)
	status := app.Command{Operation: app.Status}
	if _, err := Call(t.Context(), dir, status); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("status before launch: %v", err)
	}

	process := exec.Command(os.Args[0], "-test.run=^TestOwnerProcess$")
	process.Env = append(os.Environ(), "TMS_TEST_OWNER_DIR="+dir)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			process.Process.Kill()
			process.Wait()
		}
	})
	first := waitForOwner(t, dir)
	if second, err := Acquire(dir); !errors.Is(err, ErrOwnerRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second owner start: %v", err)
	}
	if current, err := readEndpoint(dir); err != nil || current != first {
		t.Fatalf("second launch disturbed the first endpoint: %v", err)
	}

	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := process.Wait(); !errors.As(err, &exit) {
		t.Fatalf("terminated owner returned %v", err)
	}
	if current, err := readEndpoint(dir); err != nil || current != first {
		t.Fatalf("crash did not leave a stale endpoint: %v", err)
	}
	if _, err := Call(t.Context(), dir, status); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("status after owner exit: %v", err)
	}

	replacement := startOwner(t, dir, func(context.Context, app.Command) (app.Result, error) {
		return app.Result{Revision: 13}, nil
	})
	if replacement.Token == first.Token {
		t.Fatal("replacement owner reused the old token")
	}
	if result, err := Call(t.Context(), dir, status); err != nil || result.Revision != 13 {
		t.Fatalf("status from replacement owner = %+v, %v", result, err)
	}
}

func TestCallRejectsEndpointForPreviousProcessInstance(t *testing.T) {
	dir := privateStateDir(t)
	listener, err := net.Listen("tcp4", loopback+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pid := os.Getpid()
	start, err := processStartTime(pid)
	if err != nil {
		t.Fatal(err)
	}
	stale := endpoint{
		PID: pid, StartedAtNS: start - int64(time.Second),
		Port: listener.Addr().(*net.TCPAddr).Port, Token: strings.Repeat("0", 64),
	}
	if err := publishEndpoint(dir, stale); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := Call(ctx, dir, app.Command{Operation: app.Status}); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("status from stale process instance: %v", err)
	}
}

func TestOwnerProcess(t *testing.T) {
	dir := os.Getenv("TMS_TEST_OWNER_DIR")
	if dir == "" {
		return
	}
	owner, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := owner.Serve(t.Context(), func(context.Context, app.Command) (app.Result, error) {
		return app.Result{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRPCRejectsUnauthenticatedAndInvalidRequests(t *testing.T) {
	var handled atomic.Int32
	value := startOwner(t, privateStateDir(t), func(context.Context, app.Command) (app.Result, error) {
		handled.Add(1)
		return app.Result{}, nil
	})

	baseline := handled.Load()
	valid := `{"version":1,"command":{"operation":"status"}}`
	tests := []struct {
		name string
		auth string
		body string
		code int
	}{
		{"missing token", "", valid, http.StatusUnauthorized},
		{"invalid token", "Bearer " + strings.Repeat("0", 64), valid, http.StatusUnauthorized},
		{"version mismatch", "Bearer " + value.Token, strings.Replace(valid, "1", "2", 1), http.StatusBadRequest},
		{"oversized body", "Bearer " + value.Token, valid + strings.Repeat(" ", maxRequestBytes), http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			url := "http://" + net.JoinHostPort(loopback, strconv.Itoa(value.Port)) + commandPath
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if test.auth != "" {
				req.Header.Set("Authorization", test.auth)
			}
			reply, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer reply.Body.Close()
			if reply.StatusCode != test.code {
				t.Fatalf("got HTTP %d, want %d", reply.StatusCode, test.code)
			}
		})
	}
	if handled.Load() != baseline {
		t.Fatal("invalid request reached the control handler")
	}
}

func TestRPCReportsCommittedState(t *testing.T) {
	dir := privateStateDir(t)
	path := filepath.Join(dir, "state.db")
	state, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })

	revision, err := state.SavePublication(t.Context(), 0, domain.Publication{
		ID: "news", State: domain.Enabled, Interval: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err = state.SetChatDisabled(t.Context(), "group:42", true)
	if err != nil {
		t.Fatal(err)
	}
	startOwner(t, dir, app.NewHandler(state))
	result, err := Call(t.Context(), dir, app.Command{Operation: app.Stop, PublicationID: "news"})
	if err != nil || result.Revision <= revision {
		t.Fatalf("stop = %+v, %v", result, err)
	}

	reopened, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := reopened.Status(t.Context())
	if err != nil || persisted.Revision != result.Revision ||
		len(persisted.Publications) != 1 || persisted.Publications[0].State != domain.Stopped {
		t.Fatalf("committed state after acknowledgement = %+v, %v", persisted, err)
	}
	status, err := Call(t.Context(), dir, app.Command{Operation: app.Status})
	if err != nil || len(status.Publications) != 1 || status.Publications[0].Reason != app.ReasonManualStop ||
		len(status.DisabledChats) != 1 || status.DisabledChats[0] != (app.DisabledChatStatus{ID: "group:42", Reason: app.ReasonChatDisabled}) {
		t.Fatalf("status reasons = %+v, %v", status, err)
	}
	if _, err := Call(t.Context(), dir, app.Command{Operation: app.Stop, PublicationID: "missing"}); !errors.Is(err, app.ErrUnknownPublication) {
		t.Fatalf("stop unknown publication = %v", err)
	}
}

func startOwner(t *testing.T, dir string, handler app.Handler) endpoint {
	t.Helper()
	owner, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- owner.Serve(ctx, handler)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		if err := owner.Close(); err != nil {
			t.Error(err)
		}
	})
	return waitForOwner(t, dir)
}

func waitForOwner(t *testing.T, dir string) endpoint {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for time.Now().Before(deadline) {
		value, err := readEndpoint(dir)
		if err == nil {
			if _, err := Call(ctx, dir, app.Command{Operation: app.Status}); err == nil {
				if current, err := readEndpoint(dir); err == nil && value == current {
					return value
				}
			}
		} else if !errors.Is(err, ErrAppClosed) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("owner did not publish a live endpoint")
	return endpoint{}
}

func privateStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
