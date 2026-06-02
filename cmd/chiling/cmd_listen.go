package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/lidessen/chiling/internal/shared"
)

// cmdListen implements the "listen" command, subscribing to the daemon's live
// SSE event stream and printing each event as JSON on stdout.
func cmdListen(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.Parse(args)

	resp, err := doRequest(cfg, "GET", "/v1/events", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	// SSE parsing: read lines, look for "data:" prefixed lines, parse JSON.
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var event shared.Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		data, _ := json.Marshal(event)
		fmt.Println(string(data))
	}
	return scanner.Err()
}
