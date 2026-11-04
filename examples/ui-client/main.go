// Command ui-client prints a snapshot of the trajectory and then each live
// event. It is the smallest stand-in for a future native view: one goroutine
// updates what the printer shows, and the printer does not talk to the daemon
// itself except through pkg/client.
//
//	go run ./examples/ui-client
//	go run ./examples/ui-client --ensure
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

func main() {
	ensure := flag.Bool("ensure", false, "start chilingd if it is not running")
	limit := flag.Int("limit", 20, "trajectory steps to print before following the stream")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := client.New(client.Options{})
	if *ensure {
		if err := c.EnsureDaemon(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
			os.Exit(1)
		}
	}

	events, err := c.Subscribe(ctx, client.EventFilter{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "subscribe: %v\n", err)
		os.Exit(1)
	}

	page, err := c.Activity(ctx, "", "", *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "activity: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("trajectory %d\n", len(page.Items))
	for _, step := range page.Items {
		line, _ := json.Marshal(step)
		fmt.Printf("step %s\n", line)
	}

	for ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		fmt.Printf("event %s\n", line)
	}
}
