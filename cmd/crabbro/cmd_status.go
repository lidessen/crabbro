package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/lidessen/crabbro/internal/shared"
)

// cmdStatus implements the "status" command, querying daemon health and
// configuration.
func cmdStatus(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	pretty := fs.Bool("pretty", false, "Human-readable output")
	fs.Parse(args)

	resp, err := doRequest(cfg, "GET", "/v1/status", nil)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	var status shared.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if *pretty {
		fmt.Printf("Version:       %s\n", status.Version)
		fmt.Printf("Uptime:        %s\n", formatDuration(status.UptimeSeconds))
		fmt.Printf("Proxy Addr:    %s\n", status.ProxyAddr)
		fmt.Printf("Requests:      %d\n", status.RequestCount)
		fmt.Printf("Rules:         %d\n", status.RuleCount)
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(status)
	}
	return nil
}
