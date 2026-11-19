package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"telegram-messages-sender/internal/app"
	"telegram-messages-sender/internal/control"
)

func run(args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: tmsctl status")
	}
	dir, err := control.StateDir()
	if err != nil {
		return err
	}
	result, err := control.Call(context.Background(), dir, app.Command{Operation: app.Status})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
