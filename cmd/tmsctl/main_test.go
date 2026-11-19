package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/dispatch"
	"telegram-messages-sender/internal/domain"
	"telegram-messages-sender/internal/store"
)

func TestParseCommandRequiresExplicitStableTargets(t *testing.T) {
	for _, test := range []struct {
		args []string
		want app.Command
	}{
		{[]string{"status"}, app.Command{Operation: app.Status}},
		{[]string{"start", "b-19"}, app.Command{Operation: app.Start, PublicationID: "b-19"}},
		{[]string{"stop", "a-07"}, app.Command{Operation: app.Stop, PublicationID: "a-07"}},
		{[]string{"change-interval", "a-07", "20m"}, app.Command{Operation: app.ChangeInterval, PublicationID: "a-07", Interval: 20 * time.Minute}},
		{[]string{"re-enable-chat", "supergroup:9"}, app.Command{Operation: app.ReenableChat, ChatID: "supergroup:9"}},
	} {
		if got, err := parseCommand(test.args); err != nil || got != test.want {
			t.Fatalf("parse %q = %+v, %v", test.args, got, err)
		}
	}
	for _, args := range [][]string{{}, {"status", "extra"}, {"start"}, {"stop", " "}, {"change-interval", "a", "0s"},
		{"change-interval", "a", "-1m"}, {"change-interval", "a", "20"}, {"re-enable-chat"}} {
		if _, err := parseCommand(args); err == nil {
			t.Fatalf("accepted invalid args %q", args)
		}
	}
}

func TestExecuteReportsCommittedTargetAndOwnerErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "private")
	owner, err := control.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	state, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	engine := dispatch.New(state)
	var revision int64
	for _, id := range []domain.PublicationID{"a", "b"} {
		revision, err = engine.SavePublication(ctx, revision, domain.Publication{ID: id, State: domain.Enabled, Interval: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
	}
	ownerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- owner.Serve(ownerCtx, engine.Handler()) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Errorf("owner shutdown: %v", err)
		}
	}()
	var output bytes.Buffer
	wait := time.NewTicker(5 * time.Millisecond)
	defer wait.Stop()
	for {
		if err := execute(ctx, dir, app.Command{Operation: app.Status}, &output); err == nil {
			break
		}
		select {
		case <-wait.C:
		case <-ctx.Done():
			t.Fatal("loopback owner was not ready")
		}
	}
	output.Reset()
	stopCommand, _ := parseCommand([]string{"stop", "b"})
	if err := execute(ctx, dir, stopCommand, &output); err != nil {
		t.Fatal(err)
	}
	var result app.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Revision <= revision {
		t.Fatalf("no committed revision in output: %s (%v)", output.String(), err)
	}
	snapshot, err := state.Snapshot(ctx)
	if err != nil || snapshot.Publications[0].State != domain.Enabled || snapshot.Publications[1].State != domain.Stopped {
		t.Fatalf("command did not target only b: %+v, %v", snapshot, err)
	}

	output.Reset()
	stopCommand.PublicationID = "missing"
	if err := execute(ctx, dir, stopCommand, &output); !errors.Is(err, app.ErrUnknownPublication) || output.Len() != 0 {
		t.Fatalf("owner error reported success: %v, %q", err, output.String())
	}
	if err := execute(ctx, dir, app.Command{Operation: app.ReenableChat, ChatID: "group:12"}, &output); !errors.Is(err, app.ErrVerificationRequired) {
		t.Fatalf("owner verification error lost: %v", err)
	}
}
