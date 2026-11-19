package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/control"
	"telegram-messages-sender/internal/domain"
)

var errUsage = errors.New("usage: tmsctl status | start <publication-id> | stop <publication-id> | change-interval <publication-id> <duration> | re-enable-chat <chat-id>")

func parseCommand(args []string) (app.Command, error) {
	if len(args) == 0 {
		return app.Command{}, errUsage
	}
	command := app.Command{Operation: app.Operation(args[0])}
	if command.Operation == app.Status && len(args) == 1 {
		return command, nil
	}
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return app.Command{}, errUsage
	}
	switch command.Operation {
	case app.Start, app.Stop:
		if len(args) == 2 {
			command.PublicationID = domain.PublicationID(args[1])
			return command, nil
		}
	case app.ChangeInterval:
		if len(args) == 3 {
			interval, err := time.ParseDuration(args[2])
			if err != nil || interval <= 0 {
				return app.Command{}, errors.New("interval must be a positive duration such as 20m")
			}
			command.PublicationID, command.Interval = domain.PublicationID(args[1]), interval
			return command, nil
		}
	case app.ReenableChat:
		if len(args) == 2 {
			command.ChatID = domain.ChatID(args[1])
			return command, nil
		}
	}
	return app.Command{}, errUsage
}

func execute(ctx context.Context, dir string, command app.Command, output io.Writer) error {
	result, err := control.Call(ctx, dir, command)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func run(args []string) error {
	command, err := parseCommand(args)
	if err != nil {
		return err
	}
	dir, err := control.StateDir()
	if err != nil {
		return err
	}
	return execute(context.Background(), dir, command, os.Stdout)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
