package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lidessen/chiling/pkg/client"
)

// cmdListen prints the daemon's live event stream as JSON. It reconnects
// until interrupted, resuming from the last event id it printed.
func cmdListen(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	events, err := cfg.apiClient().Subscribe(ctx, client.EventFilter{})
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	for event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			continue
		}
		fmt.Println(string(data))
	}
	return nil
}
