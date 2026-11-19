package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/store"
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

	return owner.Serve(ctx, app.NewHandler(state))
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
