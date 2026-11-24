# Crabbro

Crabbro is a local HTTP proxy that an agent drives and a person can inspect.
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

On macOS that replaces `/Applications/Crabbro.app` — the MyGo window is the app's executable, with the CLI and daemon beside it — and links `crabbro` onto `PATH`. On Linux it installs the CLI into `~/.local/bin` or `/usr/local/bin`. It does not trust the MITM CA or change the system proxy. After it finishes:

```sh
crabbro daemon start
crabbro mitm install-ca
```

Useful flags: `--app-dir ~/Applications`, `--prefix ~/.local/bin`, `--no-cli-link`, `--dry-run`, `--uninstall`.

`go install` still works if you only want the binaries on `PATH`:

```sh
go install github.com/lidessen/crabbro/cmd/crabbro@latest
go install github.com/lidessen/crabbro/cmd/crabbrod@latest
```

`crabbro daemon start` looks for `crabbrod` beside the CLI binary, then on `PATH`.

## Migrating from Chiling

This project was renamed from Chiling on 2026-11-18; the old name remains only in git history. To carry local state over, move the config directory and update names the old version wrote into files:

```sh
mv ~/.chiling ~/.crabbro
```

- Rule pipelines: `uses: chiling.<action>` becomes `uses: crabbro.<action>`.
- Plugin manifests: the `"chiling"` object in `package.json` becomes `"crabbro"`.
- Environment variables: `CHILING_*` becomes `CRABBRO_*`; schema URIs move from `chiling.dev` to `crabbro.dev`.
- MITM: the CA subject changed. Remove the old `chiling root CA` from the system trust store, then run `crabbro mitm install-ca` to install the new one.
- HTTP headers: `X-Chiling-*` becomes `X-Crabbro-*`.

## Agent workflow

JSON is the default output. Add `--pretty` where a command supports it for a short text view.

```sh
# 1. Record. The daemon proxies on localhost:8080.
crabbro daemon start --proxy-addr localhost:8080
crabbro record start --name login-hunt

# Point the client under test at the proxy, then reproduce the call.
export HTTP_PROXY=http://localhost:8080 HTTPS_PROXY=http://localhost:8080

# 2. Inspect. IDs are stable (`req_1`, …). A unique prefix works.
crabbro requests search --method POST --path /v1/login
crabbro requests show req_1 --include body,headers,rule-trace

# Optional: ask a configured decision model which flow matches a sentence.
crabbro find "the login request that returns the user token"

# 3–4. Write a rule from that capture and apply it (create enables it).
crabbro rules from req_1 --action mock --name mock-login

# 5. Replay the captured request through the proxy and check the outcome.
crabbro verify replay req_1 --expect-status 200 --expect-body-contains ok

crabbro record stop
crabbro daemon stop
```

`rules from` scaffolds a YAML pipeline from the captured host, method, and
path. `rules draft` asks the decision API to pick or write a rule. Both are
optional relative to hand-writing a file:

```sh
crabbro rules create ./mock-login.yaml --name mock-login --priority 100
crabbro rules test ./mock-login.yaml --fixture login-ok
```

## HTTPS

MITM is off until you enable it. An empty allowlist intercepts nothing; add a
pattern (a bare `*` is explicit).

```sh
crabbro daemon start --mitm --mitm-allow '*.example.com'
crabbro mitm status
crabbro mitm cert --output ~/.crabbro/certs/rootCA.pem
crabbro mitm install-ca
```

`install-ca` prints the OS command, asks for confirmation, and installs the
root CA (`crabbro root CA`) into the system trust store on macOS or Linux.
Domains on the denylist stay opaque TCP tunnels. A failed MITM handshake is
not downgraded to a tunnel.

Decrypted TLS advertises `h2` and `http/1.1`. When the client negotiates
HTTP/2, that tunnel is served as HTTP/2 and each stream still goes through the
same exchange pipeline. Clients that do not offer `h2` stay on HTTP/1.1.
WebSocket upgrades (`ws://` and `wss://`) are relayed after the 101, with
optional frame rules. WebSocket over HTTP/2 (extended CONNECT) is not
implemented.

## LLM & Decision Gateway

`crabbrod` embeds an API-key LLM & Decision Gateway on `127.0.0.1:8081` (`--gateway-addr`, or `off` to disable) alongside the forward HTTP proxy. It exposes six wire protocols (with optional `/p/{profile}` path scoping):

- **Generative (`family: generative`)**:
  - `openai-chat`: `POST /v1/chat/completions`
  - `openai-responses`: `POST /v1/responses`
  - `anthropic`: `POST /v1/messages`, `POST /v1/messages/count_tokens`
  - `gemini`: `POST /v1beta/models/{model}:generateContent`, `:streamGenerateContent`
- **Decision (`family: decision`)**:
  - `typesafe-jev`: `POST /v1/systemone` (`noul`, `choice`, `score` — routed natively to TypeSafe backends or emulated with `allowEmulation: true` over `openai-chat`, `openai-responses`, `gemini`, or `anthropic` via structured output)
  - `openai-decisions`: `POST /v1/decisions`
- **Model listing**: `GET /v1/models`

Credentials live mode `0600` in `~/.crabbro/secrets/` and are referenced by name from YAML plans and routes under `~/.crabbro/gateway/plans/*.yaml` and `~/.crabbro/gateway/routes/*.yaml` (validated with Draft 2020-12 schemas and staged/committed via `crabbro config`):

```sh
# Store credentials safely (never written into YAML files or captured history)
crabbro secrets set glm_key --from-env GLM_API_KEY
crabbro secrets set typesafe_key --from-env TYPESAFE_API_KEY

# Stage, validate, and atomically commit gateway plans & routes
crabbro config draft ./glm-plan.yaml --gateway-plan
crabbro config validate glm-plan --gateway-plan
crabbro config commit glm-plan --gateway-plan
crabbro config draft ./coding-route.yaml --gateway-route
crabbro config commit coding-route --gateway-route

# Inspect runtime status, rolling quota windows, dry-run routing, and usage ledger
crabbro gateway status --pretty
crabbro gateway plans --pretty
crabbro gateway routes --pretty
crabbro gateway preview --protocol anthropic --model coding-main --tools --pretty
crabbro gateway usage --window 24h --pretty

# Redeem a provider reset coupon (opt-in autoReset policy lives on the plan)
crabbro gateway quotas refresh
crabbro gateway quotas reset glm-coding --scope fiveHour --dry-run
```

Endpoint and model specs can normalize requests for custom gateways. For example:

```yaml
endpoints:
  anthropic:
    baseUrl: https://gateway.example.com
    requestTransform:
      stripHeaders: [anthropic-beta, 'x-stainless-*', User-Agent]
      path: /custom/messages
      query: {compat: '1', obsolete: null}
      dropBodyFields: [metadata.trace]
      renameBodyFields: {max_tokens: max_output_tokens}
models:
  example-model:
    requestTransform:
      setBodyFields: {vendor.mode: compatible}
```

Transforms run after protocol encoding, endpoint first and model second, before
credential injection. `setHeaders` overrides ordinary headers; authentication
and HTTP framing headers are protected. `path` replaces the decoded path and
`pathPrefix` prepends to it; query values replace individual keys and `null`
removes a key. Body field paths address JSON objects using dots; drops precede
renames, then explicit sets. These sets override values, unlike `extraBody`,
which supplies missing top-level defaults. Renames with multiple sources run in
lexical source order. Keep credentials in the secret store, not transforms.

Remote quota availability depends on provider credentials and APIs. Xiaomi MiMo Token Plan quota is read from `GET /v1/platform/token_plan` (`Authorization: Bearer <xiaomi_api_key>`), which reports the monthly credit allowance (`quota.total_credits` / `quota.used_credits`) and its `period.end_at` reset; a plan pinned to another `token-plan-*.xiaomimimo.com` host reuses that scheme and host. Google and OpenAI API-key plans likewise have no supported account quota endpoint here (N/A). ZenMux subscription details require a management API key, which may differ from an inference key. MiniMax quota counters and Kimi usage endpoints may change or report inconsistent values; unknown responses are not treated as fresh quota.

The `find`, `rules draft`, and plugin `decision.ask` helpers use the gateway's enabled `decision` route automatically (returning `decision API is not configured` when none is configured). See [docs/design/gateway.md](docs/design/gateway.md) for the full schema, `Quality × Cache × Quota` scoring, TTL-decay cache gravity, semantic tiering, and failover model.

## CLI reference

Global flags, before the command:

| Flag | Default | Purpose |
|---|---|---|
| `--uds-path` | `~/.crabbro/proxy.sock` | Control API Unix socket |
| `--tcp-addr` | `localhost:16379` | Control API TCP fallback |
| `--api-token` | file `api.token` next to the socket | Bearer token for TCP |

The Unix socket is mode `0600`. TCP requires `Authorization: Bearer`. The CLI
reads the token file itself.

| Command | Purpose |
|---|---|
| `daemon start [--mitm] [--mitm-allow PAT] [--proxy-addr ADDR] [--gateway-addr ADDR]` | Start `crabbrod` in the background |
| `daemon stop` | Stop the background daemon |
| `status` | Daemon status |
| `doctor [--fix] [--pretty]` | Setup checks |
| `listen` | Live events, one JSON object per line |
| `record start [--name N]` / `stop` / `status` | Recording session |
| `requests list\|search [--kind proxy\|gateway]` | Filter captured flows |
| `requests get\|show ID [--include headers,body,rule-trace] [--pretty]` | One flow. ID may be a unique prefix |
| `find "QUERY" [--pretty]` | Decision-route natural-language search |
| `verify replay ID [--expect-status N] [--expect-body-contains TEXT]` | Replay through the proxy |
| `secrets list\|set\|delete` | Manage mode-`0600` credentials in `~/.crabbro/secrets/` |
| `gateway status\|plans\|routes\|usage\|quotas\|preview\|cooldown-clear\|reload` | Gateway status, quotas, dry-run routing, and reload |
| `gateway quotas refresh` / `gateway quotas reset <plan> [--scope fiveHour\|weekly] [--dry-run]` | Refresh provider quota snapshots; redeem at most one reset coupon (opt-in `autoReset` policy per plan) |
| `config status\|draft\|validate\|commit\|discard` | Safe staging for rules, scope, `--gateway-plan`, and `--gateway-route` |
| `rules list\|get\|create\|enable\|disable\|delete\|test` | Rule CRUD and fixture test |
| `rules from ID [--action mock\|block\|header\|rewrite\|tag] [--name N] [--apply=false]` | Scaffold from a capture and create it |
| `rules draft "TEXT" [--from ID] [--apply]` | Decision-route draft |
| `rules fixture save\|list\|delete` | Saved fixtures |
| `decision status` | Whether an enabled `decision` route is configured |
| `history status` | SQLite history path, count, retention |
| `history prune` | Delete expired or overflow rows |
| `mitm status\|enable\|disable\|cert\|install-ca\|uninstall-ca` | MITM and CA |
| `mitm allow\|deny list\|add\|remove` | Domain patterns |

`requests list` filters: `--since` (request id, RFC3339, or a duration like `5m`),
`--limit`, `--method`, `--host`, `--path`, `--status`, `--query`, `--tag`, `--session`.

`crabbrod` flags: `--proxy-addr`, `--uds-path`, `--tcp-addr`, `--ring-buffer-size`,
`--fixture-dir`, `--mitm`, `--mitm-allow`, `--mitm-deny`, `--decision-provider`,
`--decision-base-url`, `--decision-model`, `--history`, `--history-retention`,
`--history-max-rows`. The API key stays in the environment.

## Rule format

`crabbro sources list` shows apps and pages. `crabbro scope pick`, `omit`, `off`, `on`, `reset`, and `bypass` narrow the current recording session. If none is recording, the first scope command opens one and says so. Omit hides rows from `requests list` (`hiddenCount`, `hiddenReason`) without stopping forwarding or rules. `requests list --all` shows the hidden rows. `requests list --type xhr --initiator <url>` and the same flags on `find` filter further. Durable lists are `~/.crabbro/scope/allow.yaml` and `deny.yaml`, edited with `config draft/validate/commit/discard --scope`. An empty allow list does not narrow.

Every rule is a YAML document — one file per rule in `rules/`, with a
`pipeline` of `uses` stages. There is no JavaScript rule format anymore: a
script that does not parse as a rule document is rejected at creation, and
`crabbro rules migrate <file.js>` wraps an older script as a literal `run`
stage (`runtime: goja`) inside the YAML. The schema is
`schemas/rule.v1.json`. The full format, including `if`, `select`, and
observe-only `parallel`, is [docs/design/rules.md](docs/design/rules.md).

Plugins are npm packages loaded by one Node host, with a worker per active plugin. `crabbro plugin install ./pkg` requires `package-lock.json` and runs `npm ci --ignore-scripts`. A pipeline stage calls `uses: <plugin>.<capability>`. See [docs/design/plugins.md](docs/design/plugins.md).

Stages run in a fresh Goja VM only when a stage is a literal `run` — the
escape hatch inside an otherwise declarative pipeline. A `run` script has no
filesystem, network, process, or environment access, each execution has a
500ms timeout, and the whole result must validate or none of its actions
apply. Everything else is a `crabbro.*` builtin, compiled to Go. Enabled
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
      uses: crabbro.mock-response
      with:
        status: 200
        headers:
          content-type: application/json
        body: '{"ok":true,"token":"mocked"}'
```

| Stage `uses` | `with` | Effect |
|---|---|---|
| `crabbro.tag` | `tags: string[]` | Add tags on the capture |
| `crabbro.set-request-header` | `name`, `value` | Set a request header |
| `crabbro.remove-request-header` | `name` | Remove a request header |
| `crabbro.rewrite-url` | `url` (http/https, no userinfo) | Change URL, dial host, TLS name, and Host together |
| `crabbro.gateway-offload` | `protocol: responses` or `anthropic-messages`, `model: plan/model` or a gateway route model | Send a sanitized request to the local gateway and skip the subscription upstream |
| `crabbro.mock-response` | `status`, optional `headers`, optional `body` | Skip upstream |
| `crabbro.block` | `status`, optional `body` | Skip upstream with that status |
| `crabbro.abort` | none | Reject with HTTP 403 |
| `crabbro.set-response-header` | `name`, `value` | Set a response header on the way back |
| `crabbro.remove-response-header` | `name` | Remove a response header |
| `crabbro.set-response-status` | `status` | Change the response status |
| `crabbro.rewrite-response-body` | `body`, or `find` + `replace` | Replace the body, or substitute within it. Bodies over 256 KiB are left unchanged |
| `crabbro.delay` | `milliseconds` (max 30000) | Wait before the response is written |
| `crabbro.throttle` | `bytesPerSecond` | Limit how fast the response body is copied |
| `crabbro.replace-frame` | `payload` or `payloadBase64` | Replace a WebSocket text/binary frame |
| `crabbro.drop-frame` | none | Drop a WebSocket text/binary frame |

A pipeline cannot produce more than one of `crabbro.mock-response`,
`crabbro.block`, `crabbro.abort`, and `crabbro.gateway-offload`. `failureMode` on the stored rule is
`fail-open` (default) or `fail-closed`. fail-open skips a rule that errors.
fail-closed answers 403. Gateway offload requires an enabled local gateway;
invalid input or an unavailable gateway returns an error without forwarding to
the original upstream. Only allowlisted protocol fields and the configured
model enter the gateway request and proxy capture; subscription headers and
query parameters are excluded. Streaming gateway responses pass through without
response rule rewrites.

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
      uses: crabbro.replace-frame
      with:
        payload: "rewritten"
```

`ctx.frame` is `{direction, opcode, payload, fin}`. `direction` is `client` or
`server`. Control frames (ping, pong, close) are relayed unchanged. Frame
stages see each data frame on its own, not a reassembled message.

Create from a file:

```sh
crabbro rules create ./mock-login.yaml --name mock-login --priority 100
```

A `.js` file is refused with a pointer at the converter:

```sh
crabbro rules migrate ./old-rule.js --id old-rule
```

`POST /v1/rules` body: `{"name","priority","script","failureMode"}`.

## Control API

`crabbrod` serves `/v1` on the Unix socket and on localhost TCP.

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
control socket, unless `--history off`). `crabbro requests list` and
`requests show` read that store, so a restart keeps them. Retention defaults
to 7 days and 10000 rows. `crabbro history status` and `crabbro history prune`
report and apply that policy.

## The desktop UI

`cmd/crabbro-ui` is the native window, drawn by [MyGo](https://mygo.egoist.dev/docs/ui) from Go,
with no webview. It is a client of the same control API the CLI uses: `pkg/api` for the JSON
types and `pkg/client` for the calls, including `Subscribe`. Neither package imports a UI
toolkit. The contract, the event types, and the snapshot-then-follow pattern are in
[docs/design/ui-api.md](docs/design/ui-api.md).

```sh
go run ./cmd/crabbro-ui
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

A goroutine keeps the state fresh: ensure the daemon (`EnsureDaemon` starts `crabbrod` when
`/v1/status` does not answer), take a snapshot, follow the event stream, and start over when the
stream drops. The frame function only reads that state and calls client methods for clicks.
Closing the window leaves capture, rules, and MITM running.

`scripts/install.sh` builds the UI and installs it as the app's executable
(`Contents/MacOS/crabbro-ui`); when the UI does not build, the app keeps a launcher that starts
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
| `cmd/crabbrod` | Daemon |
| `cmd/crabbro` | CLI |
| `cmd/crabbro-ui` | MyGo native UI (the Crabbro.app window) |
| `pkg/api` | `/v1` JSON types shared by the daemon and clients |
| `pkg/client` | Typed control-socket client (`Subscribe`, trajectory, scope, rules) |
| `examples/ui-client` | Prints the trajectory and the live event stream |
| `docs/design/ui-api.md` | Methods, events, pagination, and how a native view consumes them |
| `scripts/install.sh` | Local install: `Crabbro.app` on macOS, CLI on Linux |
| `internal/proxy` | Listener, exchange pipeline, CONNECT, replay |
| `internal/mitm` | CA and client-facing TLS |
| `internal/rule` | YAML pipelines, Goja `run` stages, rule files |
| `internal/plugin` | Node plugin host |
| `sdk/plugin` | TypeScript types for plugin authors |
| `cmd/crabbro-plugin-host` | Node host entry |
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
