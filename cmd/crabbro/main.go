// Command crabbro is the CLI client for crabbrod. It communicates with
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

	"github.com/lidessen/crabbro/internal/shared"
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
	apiToken := flag.String("api-token", "", "Control API bearer token (default: api.token next to the socket)")
	flag.Usage = printUsage
	flag.Parse()

	if flag.NArg() < 1 {
		printUsage()
		return nil
	}

	cmd := flag.Arg(0)
	cfg := &clientConfig{
		UDS:   expandPath(*udsPath),
		TCP:   *tcpAddr,
		Token: *apiToken,
	}

	switch cmd {
	case "status":
		return cmdStatus(cfg, flag.Args()[1:])
	case "doctor":
		return cmdDoctor(cfg, flag.Args()[1:])
	case "listen":
		return cmdListen(cfg, flag.Args()[1:])
	case "requests":
		return cmdRequests(cfg, flag.Args()[1:])
	case "rules":
		return cmdRules(cfg, flag.Args()[1:])
	case "mitm":
		return cmdMITM(cfg, flag.Args()[1:])
	case "daemon":
		return cmdDaemon(cfg, flag.Args()[1:])
	case "record":
		return cmdRecord(cfg, flag.Args()[1:])
	case "find":
		return cmdFind(cfg, flag.Args()[1:])
	case "verify":
		return cmdVerify(cfg, flag.Args()[1:])
	case "decision":
		return cmdDecision(cfg, flag.Args()[1:])
	case "history":
		return cmdHistory(cfg, flag.Args()[1:])
	case "plugin":
		return cmdPlugin(cfg, flag.Args()[1:])
	case "secrets":
		return cmdSecrets(cfg, flag.Args()[1:])
	case "gateway":
		return cmdGateway(cfg, flag.Args()[1:])
	case "sources":
		return cmdSources(cfg, flag.Args()[1:])
	case "scope":
		return cmdScope(cfg, flag.Args()[1:])
	case "config":
		return cmdConfig(cfg, flag.Args()[1:])
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func printUsage() {
	fmt.Println(`Usage: crabbro <command> [options]

Commands:
  status                    Show daemon status
  doctor                    Check system setup and diagnose issues
    --fix                     Attempt to fix detected issues
    --pretty                  Human-readable output
  listen                    Subscribe to live events (SSE stream)
  daemon start              Start crabbrod in the background
  daemon stop               Stop the background daemon
  record start [--name]     Open a recording session
  record stop               Stop the current recording session
  record status             Show the current recording session
  requests list             List captured requests
    --method --host --path --status --query --tag --since --limit
  requests search           Same filters as list
  requests get <id>         Get a single request (prefix ok if unique)
  requests show <id>        Alias of get
  find "<query>"            Natural-language search via Jev or OpenAI
  verify replay <id>        Replay a captured request through the proxy
  decision status           Show whether a decision API is configured
  history status            Show the SQLite history
  history prune             Drop expired exchanges
  sources list              List apps and pages the proxy has seen
  scope status              Show the session view filter
  scope pick|omit|off|on|reset|bypass
  plugin list               List installed plugins (does not start a worker)
  plugin install <path>     Install from a directory with package-lock.json
  plugin inspect <id>       Read tools and uses (starts the worker)
  plugin invoke <p> <i> <t> Call a tool
  secrets list|set|delete   Manage mode-0600 credentials under ~/.crabbro/secrets/
  gateway status|plans|routes    Inspect multi-protocol LLM & Decision gateway
  gateway auth <login|status|logout>  Manage gateway account authentication
  gateway usage|preview|reload  View quota usage, dry-run router, or reload config
  gateway why <request-id>  Explain a recorded gateway route
  gateway cooldown-clear <plan> Clear an active plan cooldown
  config status             List rule, scope, or gateway config files and drafts
  rules list                List rules
  rules diagnose [--json]   Diagnose rule files
  rules validate <file>     Check a YAML rule without loading it
  rules migrate <file.js>   Wrap a legacy script as a run stage
  rules preview             Match rules without applying them
  rules create <file>       Create a rule from a JS file
  rules from <id>           Scaffold a rule from a captured request
  rules draft "<text>"      Ask the decision API to draft a rule
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
  mitm enable               Enable HTTPS MITM interception
  mitm disable              Disable HTTPS MITM interception
  mitm status               Show MITM status and domain patterns
  mitm cert [--output <file>] Download the root CA certificate
  mitm install-ca           Install the root CA into the system trust store
  mitm uninstall-ca         Remove the root CA from the system trust store
  mitm allow list           List allowed domain patterns
  mitm allow add <pattern>  Add a domain to the allowlist
  mitm allow remove <pattern> Remove a domain from the allowlist
  mitm deny list            List denied domain patterns
  mitm deny add <pattern>   Add a domain to the denylist
  mitm deny remove <pattern> Remove a domain from the denylist

Global options:
  --uds-path    Unix domain socket path (default: ~/.crabbro/proxy.sock)
  --tcp-addr    TCP address fallback (default: localhost:16379)
  --api-token   Bearer token for the TCP control API

Use "crabbro help" for more information.`)
}
