// Command chiling is the CLI client for chilingd. It communicates with
// the daemon over the control API (Unix domain socket or TCP) and provides
// commands for querying status, listing requests, and subscribing to events.
//
// Output defaults to JSON for agent consumption. Use --pretty for human-friendly
// output where available.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/lidessen/chiling/internal/shared"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Global flags (must appear before the subcommand).
	udsPath := flag.String("uds-path", shared.DefaultUDSPath, "Unix domain socket path")
	tcpAddr := flag.String("tcp-addr", shared.DefaultTCPAddr, "TCP address fallback")
	flag.Usage = printUsage
	flag.Parse()

	if flag.NArg() < 1 {
		printUsage()
		return nil
	}

	cmd := flag.Arg(0)
	cfg := &clientConfig{
		UDS: expandPath(*udsPath),
		TCP: *tcpAddr,
	}

	switch cmd {
	case "status":
		return cmdStatus(cfg, flag.Args()[1:])
	case "listen":
		return cmdListen(cfg, flag.Args()[1:])
	case "requests":
		return cmdRequests(cfg, flag.Args()[1:])
	case "rules":
		return cmdRules(cfg, flag.Args()[1:])
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func printUsage() {
	fmt.Println(`Usage: chiling <command> [options]

Commands:
  status                    Show daemon status
  listen                    Subscribe to live events (SSE stream)
  requests list             List captured requests
  requests get <id>         Get a single request's details
  rules list                List rules
  rules create <file>       Create a rule from a JS file
  rules get <id>            Get a rule's details
  rules enable <id>         Enable a rule
  rules disable <id>        Disable a rule
  rules delete <id>         Delete a rule
  rules test <file>         Test a rule script against a fixture
    --fixture <name>          Fixture name (required)
    --pretty                  Human-readable output
  rules fixture save ...    Save a captured request as a fixture
  rules fixture list        List saved fixtures
  rules fixture delete      Delete a fixture

Global options:
  --uds-path    Unix domain socket path (default: ~/.chiling/proxy.sock)
  --tcp-addr    TCP address fallback (default: localhost:16379)

Use "chiling help" for more information.`)
}
