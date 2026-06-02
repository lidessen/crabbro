package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// cmdRequests dispatches to request subcommands.
func cmdRequests(cfg *clientConfig, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chiling requests <list|get> [options]")
	}

	sub := args[0]
	switch sub {
	case "list":
		return cmdRequestsList(cfg, args[1:])
	case "get":
		return cmdRequestsGet(cfg, args[1:])
	default:
		return fmt.Errorf("unknown requests subcommand: %s", sub)
	}
}

// cmdRequestsList implements "requests list", querying the daemon for captured
// requests with optional filtering by time and count.
func cmdRequestsList(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("requests list", flag.ContinueOnError)
	since := fs.String("since", "", "Cursor or duration (e.g., '5m', '1h')")
	limit := fs.Int("limit", 0, "Maximum number of results")
	pretty := fs.Bool("pretty", false, "Human-readable output")
	fs.Parse(args)

	path := "/v1/requests"
	params := []string{}
	if *since != "" {
		params = append(params, "since="+*since)
	}
	if *limit > 0 {
		params = append(params, fmt.Sprintf("limit=%d", *limit))
	}
	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}

	resp, err := doRequest(cfg, "GET", path, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	var listResp shared.ListRequestsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if *pretty && len(listResp.Requests) > 0 {
		for _, req := range listResp.Requests {
			fmt.Printf("%s %s %s (%d, %dms)\n", req.ID, req.Method, req.URL, req.Status, req.DurationMs)
		}
		if listResp.NextCursor != "" {
			fmt.Printf("--- next cursor: %s ---\n", listResp.NextCursor)
		}
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(listResp)
	}
	return nil
}

// cmdRequestsGet implements "requests get <id>", fetching full details for a
// single captured request from the daemon.
func cmdRequestsGet(cfg *clientConfig, args []string) error {
	fs := flag.NewFlagSet("requests get", flag.ContinueOnError)
	include := fs.String("include", "", "Comma-separated: headers, body, rule-trace")
	pretty := fs.Bool("pretty", false, "Human-readable output")
	fs.Parse(args)

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: chiling requests get <id> [options]")
	}
	id := fs.Arg(0)

	path := "/v1/requests/" + id
	if *include != "" {
		path += "?include=" + *include
	}

	resp, err := doRequest(cfg, "GET", path, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("request %s not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
	}

	var detailResp shared.RequestDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&detailResp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}

	if *pretty {
		req := detailResp.Request
		fmt.Printf("ID:        %s\n", req.ID)
		fmt.Printf("Started:   %s\n", req.StartedAt.Format(time.RFC3339))
		if req.CompletedAt != nil {
			fmt.Printf("Completed: %s\n", req.CompletedAt.Format(time.RFC3339))
		}
		fmt.Printf("Method:    %s\n", req.Method)
		fmt.Printf("URL:       %s\n", req.URL)
		if req.Status > 0 {
			fmt.Printf("Status:    %d\n", req.Status)
		}
		if req.DurationMs > 0 {
			fmt.Printf("Duration:  %dms\n", req.DurationMs)
		}
		fmt.Printf("Client:    %s\n", req.ClientAddr)
		if len(req.Tags) > 0 {
			fmt.Printf("Tags:      %s\n", strings.Join(req.Tags, ", "))
		}
		if len(req.RuleTrace) > 0 {
			fmt.Println("Rule Traces:")
			for _, trace := range req.RuleTrace {
				fmt.Printf("  Rule %s: matched=%v, duration=%dms\n", trace.RuleID, trace.Matched, trace.DurationMs)
				if trace.Error != "" {
					fmt.Printf("    Error: %s\n", trace.Error)
				}
			}
		}
		if detailResp.Body != nil {
			fmt.Printf("Body:\n%s\n", *detailResp.Body)
		}
	} else {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(detailResp)
	}
	return nil
}
