# Chiling

Chiling is a local HTTP proxy that an agent drives and a person can inspect.
The daemon owns capture, rules, and MITM. The CLI is a client of that daemon,
and the desktop UI attaches to the same control API.

The loop the CLI is built for:

1. Record traffic.
2. Inspect the request you care about.
3. Write a rule.
4. Apply it.
5. Verify the next attempt did what you wanted.

## Install

Requires Go 1.26 or newer. From a checkout, one command builds the binaries and installs them:

```sh
./scripts/install.sh
```

On macOS that replaces `/Applications/Chiling.app` — the MyGo window is the app's executable, with the CLI and daemon beside it — and links `chiling` onto `PATH`. On Linux it installs the CLI into `~/.local/bin` or `/usr/local/bin`. It does not trust the MITM CA or change the system proxy. After it finishes:

```sh
chiling daemon start
chiling mitm install-ca
```

Useful flags: `--app-dir ~/Applications`, `--prefix ~/.local/bin`, `--no-cli-link`, `--dry-run`, `--uninstall`.

`go install` still works if you only want the binaries on `PATH`:

```sh
go install github.com/lidessen/chiling/cmd/chiling@latest
go install github.com/lidessen/chiling/cmd/chilingd@latest
```

`chiling daemon start` looks for `chilingd` beside the CLI binary, then on `PATH`.

## Agent workflow

JSON is the default output. Add `--pretty` where a command supports it for a short text view.

```sh
# 1. Record. The daemon proxies on localhost:8080.
chiling daemon start --proxy-addr localhost:8080
chiling record start --name login-hunt

# Point the client under test at the proxy, then reproduce the call.
export HTTP_PROXY=http://localhost:8080 HTTPS_PROXY=http://localhost:8080

# 2. Inspect. IDs are stable (`req_1`, …). A unique prefix works.
chiling requests search --method POST --path /v1/login
chiling requests show req_1 --include body,headers,rule-trace

# Optional: ask a configured decision model which flow matches a sentence.
chiling find "the login request that returns the user token"

# 3–4. Write a rule from that capture and apply it (create enables it).
chiling rules from req_1 --action mock --name mock-login

# 5. Replay the captured request through the proxy and check the outcome.
chiling verify replay req_1 --expect-status 200 --expect-body-contains ok

chiling record stop
chiling daemon stop
```

`rules from` scaffolds a YAML pipeline from the captured host, method, and
path. `rules draft` asks the decision API to pick or write a rule. Both are
optional relative to hand-writing a file:

```sh
chiling rules create ./mock-login.yaml --name mock-login --priority 100
chiling rules test ./mock-login.yaml --fixture login-ok
```

## HTTPS

MITM is off until you enable it. An empty allowlist intercepts nothing; add a
pattern (a bare `*` is explicit).

```sh
chiling daemon start --mitm --mitm-allow '*.example.com'
chiling mitm status
chiling mitm cert --output ~/.chiling/certs/rootCA.pem
chiling mitm install-ca
```

`install-ca` prints the OS command, asks for confirmation, and installs the
root CA (`chiling root CA`) into the system trust store on macOS or Linux.
Domains on the denylist stay opaque TCP tunnels. A failed MITM handshake is
not downgraded to a tunnel.

Decrypted TLS advertises `h2` and `http/1.1`. When the client negotiates
HTTP/2, that tunnel is served as HTTP/2 and each stream still goes through the
same exchange pipeline. Clients that do not offer `h2` stay on HTTP/1.1.
WebSocket upgrades (`ws://` and `wss://`) are relayed after the 101, with
optional frame rules. WebSocket over HTTP/2 (extended CONNECT) is not
implemented.

## Decision API

Natural-language find and rule draft call an optional decision API. With no
key, the proxy, capture, and rules still work; `find` and `rules draft` return
a clear error.

Two providers share one Go result type. Responses are unmarshaled into those
structs and rejected when a score, choice, or request id is out of range.

| Provider | What it calls | Default base URL | Default model |
|---|---|---|---|
| `jev` | TypeSafe Jev `POST /v1/systemone` (noul per flow, choice for the rule action) | `https://api.typesafe.ai` | `jev-latest` |
| `openai` | Chat Completions with strict `json_schema` | `https://api.openai.com/v1` | `gpt-4o-mini` |

`openai` is any OpenAI-compatible base URL, including third-party hosts.
Jev is the TypeSafe decision model (choice, score, noul), not a chat model.
The design docs do not name a library called “jev”; this uses the public
TypeSafe HTTP API and, for OpenAI-compatible endpoints, structured outputs
checked against Go structs.

```sh
# Jev (first-party TypeSafe, or a compatible deployment)
export CHILING_DECISION_PROVIDER=jev
export CHILING_DECISION_API_KEY="$TYPESAFE_API_KEY"   # TYPESAFE_API_KEY also works
# export CHILING_DECISION_BASE_URL=https://api.typesafe.ai
# export CHILING_DECISION_MODEL=jev-latest

# OpenAI or a third-party compatible endpoint
export CHILING_DECISION_PROVIDER=openai
export CHILING_DECISION_API_KEY="$OPENAI_API_KEY"    # OPENAI_API_KEY also works
# export CHILING_DECISION_BASE_URL=https://api.openai.com/v1
# export OPENAI_BASE_URL=https://your-host/v1
# export CHILING_DECISION_MODEL=gpt-4o-mini

chilingd   # the daemon reads the environment; the key is not a CLI flag
chiling decision status
chiling find "the login request that returns the user token"
chiling rules draft "mock the login response" --from req_1 --apply
```

If only `TYPESAFE_API_KEY` is set, the provider defaults to `jev`. Otherwise it
defaults to `openai`. `chiling decision status` never prints the key.

## CLI reference

Global flags, before the command:

| Flag | Default | Purpose |
|---|---|---|
| `--uds-path` | `~/.chiling/proxy.sock` | Control API Unix socket |
| `--tcp-addr` | `localhost:16379` | Control API TCP fallback |
| `--api-token` | file `api.token` next to the socket | Bearer token for TCP |

The Unix socket is mode `0600`. TCP requires `Authorization: Bearer`. The CLI
reads the token file itself.

| Command | Purpose |
|---|---|
| `daemon start [--mitm] [--mitm-allow PAT] [--proxy-addr ADDR]` | Start `chilingd` in the background |
| `daemon stop` | Stop the background daemon |
| `status` | Daemon status |
| `doctor [--fix] [--pretty]` | Setup checks |
| `listen` | Live events, one JSON object per line |
| `record start [--name N]` / `stop` / `status` | Recording session |
| `requests list\|search` | Filter captured flows |
| `requests get\|show ID [--include headers,body,rule-trace] [--pretty]` | One flow. ID may be a unique prefix |
| `find "QUERY" [--pretty]` | Decision-API search |
| `verify replay ID [--expect-status N] [--expect-body-contains TEXT]` | Replay through the proxy |
| `rules list\|get\|create\|enable\|disable\|delete\|test` | Rule CRUD and fixture test |
| `rules from ID [--action mock\|block\|header\|rewrite\|tag] [--name N] [--apply=false]` | Scaffold from a capture and create it |
| `rules draft "TEXT" [--from ID] [--apply]` | Decision-API draft |
| `rules fixture save\|list\|delete` | Saved fixtures |
| `decision status` | Whether a decision provider is configured |
| `history status` | SQLite history path, count, retention |
| `history prune` | Delete expired or overflow rows |
| `mitm status\|enable\|disable\|cert\|install-ca\|uninstall-ca` | MITM and CA |
| `mitm allow\|deny list\|add\|remove` | Domain patterns |

`requests list` filters: `--since` (request id, RFC3339, or a duration like `5m`),
`--limit`, `--method`, `--host`, `--path`, `--status`, `--q`, `--tag`, `--session`.

`chilingd` flags: `--proxy-addr`, `--uds-path`, `--tcp-addr`, `--ring-buffer-size`,
`--fixture-dir`, `--mitm`, `--mitm-allow`, `--mitm-deny`, `--decision-provider`,
`--decision-base-url`, `--decision-model`, `--history`, `--history-retention`,
`--history-max-rows`. The API key stays in the environment.

## Rule format

`chiling sources list` shows apps and pages. `chiling scope pick`, `omit`, `off`, `on`, `reset`, and `bypass` narrow the current recording session. If none is recording, the first scope command opens one and says so. Omit hides rows from `requests list` (`hiddenCount`, `hiddenReason`) without stopping forwarding or rules. `requests list --all` shows the hidden rows. `requests list --type xhr --initiator <url>` and the same flags on `find` filter further. Durable lists are `~/.chiling/scope/allow.yaml` and `deny.yaml`, edited with `config draft/validate/commit/discard --scope`. An empty allow list does not narrow.

Every rule is a YAML document — one file per rule in `rules/`, with a
`pipeline` of `uses` stages. There is no JavaScript rule format anymore: a
script that does not parse as a rule document is rejected at creation, and
`chiling rules migrate <file.js>` wraps an older script as a literal `run`
stage (`runtime: goja`) inside the YAML. The schema is
`schemas/rule.v1.json`. The full format, including `if`, `select`, and
observe-only `parallel`, is [docs/design/rules.md](docs/design/rules.md).

Plugins are npm packages loaded by one Node host, with a worker per active plugin. `chiling plugin install ./pkg` requires `package-lock.json` and runs `npm ci --ignore-scripts`. A pipeline stage calls `uses: <plugin>.<capability>`. See [docs/design/plugins.md](docs/design/plugins.md).

Stages run in a fresh Goja VM only when a stage is a literal `run` — the
escape hatch inside an otherwise declarative pipeline. A `run` script has no
filesystem, network, process, or environment access, each execution has a
500ms timeout, and the whole result must validate or none of its actions
apply. Everything else is a `chiling.*` builtin, compiled to Go. Enabled
rules run in ascending priority, then rule id. A later rule sees request
patches from earlier rules. The original URL is kept on the capture.

The login mock from the JS days, as the pipeline that replaces it:

```yaml
version: 1
id: mock-login
description: Synthetic login response
enabled: true
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - id: mock
      uses: chiling.mock-response
      with:
        status: 200
        headers:
          content-type: application/json
        body: '{"ok":true,"token":"mocked"}'
```

| Stage `uses` | `with` | Effect |
|---|---|---|
| `chiling.tag` | `tags: string[]` | Add tags on the capture |
| `chiling.set-request-header` | `name`, `value` | Set a request header |
| `chiling.remove-request-header` | `name` | Remove a request header |
| `chiling.rewrite-url` | `url` (http/https, no userinfo) | Change URL, dial host, TLS name, and Host together |
| `chiling.mock-response` | `status`, optional `headers`, optional `body` | Skip upstream |
| `chiling.block` | `status`, optional `body` | Skip upstream with that status |
| `chiling.abort` | none | Reject with HTTP 403 |
| `chiling.set-response-header` | `name`, `value` | Set a response header on the way back |
| `chiling.remove-response-header` | `name` | Remove a response header |
| `chiling.set-response-status` | `status` | Change the response status |
| `chiling.rewrite-response-body` | `body`, or `find` + `replace` | Replace the body, or substitute within it. Bodies over 256 KiB are left unchanged |
| `chiling.delay` | `milliseconds` (max 30000) | Wait before the response is written |
| `chiling.throttle` | `bytesPerSecond` | Limit how fast the response body is copied |
| `chiling.replace-frame` | `payload` or `payloadBase64` | Replace a WebSocket text/binary frame |
| `chiling.drop-frame` | none | Drop a WebSocket text/binary frame |

A pipeline cannot produce more than one of `chiling.mock-response`,
`chiling.block`, and `chiling.abort`. `failureMode` on the stored rule is
`fail-open` (default) or `fail-closed`. fail-open skips a rule that errors.
fail-closed answers 403.

A `run` stage returns `{match, actions}` against the same `ctx` the old
script rules saw (`ctx.request`, `ctx.response` with `status`, `headers`,
`body`, `bodyTruncated`, and `ctx.frame` for WebSocket data frames). The
frame phase of a document does what the JS upgrade-match dance did:

```yaml
version: 1
id: ws-rewrite
on:
  request:
    path: /ws
frame:
  pipeline:
    - id: rewrite-client-text
      if: "frame.direction == 'client' && frame.opcode == 'text'"
      uses: chiling.replace-frame
      with:
        payload: "rewritten"
```

`ctx.frame` is `{direction, opcode, payload, fin}`. `direction` is `client` or
`server`. Control frames (ping, pong, close) are relayed unchanged. Frame
stages see each data frame on its own, not a reassembled message.

Create from a file:

```sh
chiling rules create ./mock-login.yaml --name mock-login --priority 100
```

A `.js` file is refused with a pointer at the converter:

```sh
chiling rules migrate ./old-rule.js --id old-rule
```

`POST /v1/rules` body: `{"name","priority","script","failureMode"}`.

## Control API

`chilingd` serves `/v1` on the Unix socket and on localhost TCP.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/status` | Version, proxy address, counts |
| `GET` | `/v1/events` | Server-sent events. `id` lines, resume with `after` or `Last-Event-ID` |
| `GET` | `/v1/requests` | List. Query: `since`, `sinceTime`, `limit`, `method`, `host`, `path`, `status`, `q`, `tag`, `session` |
| `GET` | `/v1/requests/{id}` | Detail. `include=headers,body,rule-trace`. `body` is the request body, `responseBody` is the response body |
| `GET` `POST` | `/v1/sessions` | List or start a recording |
| `GET` | `/v1/sessions/current` | Active recording |
| `POST` | `/v1/sessions/{id}/stop` | Stop a recording |
| `GET` | `/v1/activity` | Trajectory tail (`session`, `limit`) |
| `GET` | `/v1/trajectory` | Oldest-first pages (`session`, `cursor`, `limit`) |
| `POST` | `/v1/verify/replay` | Replay a capture |
| `POST` | `/v1/find` | Decision search. `{query, limit}` |
| `GET` | `/v1/decision` | Provider status, no secret |
| `GET` | `/v1/history` | History status |
| `POST` | `/v1/history/prune` | Apply retention |
| `POST` | `/v1/rules/draft` | Decision draft. `{description, requestId, apply}` |
| CRUD | `/v1/rules`, `/v1/fixtures`, `/v1/mitm/...` | Rules, fixtures, MITM |

Errors:

```json
{"error":{"code":"NOT_FOUND","message":"request not found","details":[{"field":"id","message":"..."}]}}
```

Events are best-effort. Slow subscribers can miss them. The request list is
the source for recent traffic. Bodies are previews (64 KiB each, 10 MiB
shared). Forwarding is not truncated to that cap.

Completed exchanges are also written to SQLite (`history.db` next to the
control socket, unless `--history off`). `chiling requests list` and
`requests show` read that store, so a restart keeps them. Retention defaults
to 7 days and 10000 rows. `chiling history status` and `chiling history prune`
report and apply that policy.

## The desktop UI

`cmd/chiling-ui` is the native window, drawn by [MyGo](https://mygo.egoist.dev/docs/ui) from Go,
with no webview. It is a client of the same control API the CLI uses: `pkg/api` for the JSON
types and `pkg/client` for the calls, including `Subscribe`. Neither package imports a UI
toolkit. The contract, the event types, and the snapshot-then-follow pattern are in
[docs/design/ui-api.md](docs/design/ui-api.md).

```sh
go run ./cmd/chiling-ui
```

The window has four pages:

- **Requests** — the live table (newest first, search, hidden rows) and, below it, the detail of
  the request chosen: headers, bodies, and the rule trace. A replay button sends it back through
  the proxy.
- **Rules** — the list, with enable, disable, delete, and an editor that switches between a visual form
  (match, description, priority, and the builtin stages with their parameters) and the raw YAML code; rules
  the form cannot express — hooks, `vars`, `select`, `parallel`, plugin stages — open in code. Saving goes
  through `PUT /v1/rules/{id}`.
- **Trajectory** — the timeline tail, newest first; a step's request id opens it in Requests.
- **Daemon** — status, the recording session, history, MITM, and the scope.

A goroutine keeps the state fresh: ensure the daemon (`EnsureDaemon` starts `chilingd` when
`/v1/status` does not answer), take a snapshot, follow the event stream, and start over when the
stream drops. The frame function only reads that state and calls client methods for clicks.
Closing the window leaves capture, rules, and MITM running.

`scripts/install.sh` builds the UI and installs it as the app's executable
(`Contents/MacOS/chiling-ui`); when the UI does not build, the app keeps a launcher that starts
the daemon. MyGo's own packaging (`mygo.json`, signing, notarization, auto-update) can replace
that script for the app later; the script stays until then.

`examples/ui-client` prints the same feed as the window, from the same packages, without a UI
toolkit:

```sh
go run ./examples/ui-client
```

## What is still not in this tree

- Tab and initiator attribution from a browser extension or Chrome DevTools Protocol. Header-derived pages and `Sec-Fetch-*` resource types are implemented. See [docs/design/inspect.md](docs/design/inspect.md).
- A separate per-exchange memory budget beyond the capture and 256 KiB transform caps.
- Trust-store detection. `mitmCertInstalled` is still whatever the CLI reports after install.
- WebSocket message reassembly. Frame rules see one data frame at a time and do not run on ping, pong, or close.
- WebSocket over HTTP/2 (extended CONNECT).
- Response rewrites of bodies larger than 256 KiB. Those responses are forwarded unchanged and the rule records an error.

## Layout

| Path | Role |
|---|---|
| `cmd/chilingd` | Daemon |
| `cmd/chiling` | CLI |
| `cmd/chiling-ui` | MyGo native UI (the Chiling.app window) |
| `pkg/api` | `/v1` JSON types shared by the daemon and clients |
| `pkg/client` | Typed control-socket client (`Subscribe`, trajectory, scope, rules) |
| `examples/ui-client` | Prints the trajectory and the live event stream |
| `docs/design/ui-api.md` | Methods, events, pagination, and how a native view consumes them |
| `scripts/install.sh` | Local install: `Chiling.app` on macOS, CLI on Linux |
| `internal/proxy` | Listener, exchange pipeline, CONNECT, replay |
| `internal/mitm` | CA and client-facing TLS |
| `internal/rule` | YAML pipelines, Goja `run` stages, rule files |
| `internal/plugin` | Node plugin host |
| `sdk/plugin` | TypeScript types for plugin authors |
| `cmd/chiling-plugin-host` | Node host entry |
| `internal/decision` | Jev and OpenAI-compatible decision calls |
| `internal/api` | `/v1` control API |
| `internal/session` | Recording sessions and trajectory |
| `internal/history` | SQLite exchange log |
| `internal/ring` | Recent flows |
| `docs/design/rules.md` | YAML rule format (`pipeline`, `if`, `select`, observe-only `parallel`) |
| `docs/design/inspect.md` | Traffic sources, session scope, durable allow/deny |
| `internal/inspect` | Attribution, scope matching, Linux process lookup |
| `schemas/rule.v1.json` | Proposed JSON Schema for that format |
| `docs/design/plugins.md` | Node plugin host: one process, a worker per active plugin |
