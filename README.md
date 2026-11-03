# Chiling (敕令)

Chiling is a local HTTP proxy that an agent drives and a person can inspect.
The daemon owns capture, rules, and MITM. The CLI is a client of that daemon.
A desktop UI can attach later to the same control API.

The loop the CLI is built for:

1. Record traffic.
2. Inspect the request you care about.
3. Write a rule.
4. Apply it.
5. Verify the next attempt did what you wanted.

## Install

Requires Go 1.26 or newer.

```sh
go install github.com/lidessen/chiling/cmd/chiling@latest
go install github.com/lidessen/chiling/cmd/chilingd@latest
```

From a checkout:

```sh
go build -o chiling ./cmd/chiling
go build -o chilingd ./cmd/chilingd
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

`rules from` scaffolds JavaScript from the captured host, method, and path.
`rules draft` asks the decision API to pick or write a rule. Both are optional
relative to hand-writing a file:

```sh
chiling rules create ./mock-login.js --name mock-login --priority 100
chiling rules test ./mock-login.js --fixture login-ok
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
| `mitm status\|enable\|disable\|cert\|install-ca\|uninstall-ca` | MITM and CA |
| `mitm allow\|deny list\|add\|remove` | Domain patterns |

`requests list` filters: `--since` (request id, RFC3339, or a duration like `5m`),
`--limit`, `--method`, `--host`, `--path`, `--status`, `--q`, `--tag`, `--session`.

`chilingd` flags: `--proxy-addr`, `--uds-path`, `--tcp-addr`, `--ring-buffer-size`,
`--fixture-dir`, `--mitm`, `--mitm-allow`, `--mitm-deny`, `--decision-provider`,
`--decision-base-url`, `--decision-model`. The API key stays in the environment.

## Rule format

Rules are JavaScript. The daemon runs them in a fresh Goja VM with no
filesystem, network, process, or environment access. Each evaluation has a
500ms timeout. The whole result must validate or none of its actions apply.
Enabled rules run in ascending priority, then rule id. A later rule sees
request patches from earlier rules. The original URL is kept on the capture.

```js
export default function rule(ctx) {
  if (ctx.request.host !== "api.example.com") return { match: false };
  if (ctx.request.path !== "/v1/login") return { match: false };
  return {
    match: true,
    actions: [
      {
        type: "mockResponse",
        params: {
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ ok: true })
        }
      }
    ]
  };
}
```

Flat action fields (`status`, `body`, `headers`, `name`, `value`, `url`, `tags`
next to `type`) are accepted and lifted into `params`.

| Action | Params | Effect |
|---|---|---|
| `tag` | `tags: string[]` | Add tags on the capture |
| `setRequestHeader` | `name`, `value` | Set a request header |
| `removeRequestHeader` | `name` | Remove a request header |
| `rewriteURL` | `url` (http/https, no userinfo) | Change URL, dial host, TLS name, and Host together |
| `mockResponse` | `status`, optional `headers`, optional `body` | Skip upstream |
| `block` | `status`, optional `body` | Skip upstream with that status |
| `abort` | none | Reject with HTTP 403. Replaces the old silent close |

A rule cannot return more than one of `mockResponse`, `block`, and `abort`.
`failureMode` on the stored rule is `fail-open` (default) or `fail-closed`.
fail-open skips a rule that errors. fail-closed answers 403.

Create from a file or inline JSON:

```sh
chiling rules create ./rule.js --name mock-login --priority 100
```

`POST /v1/rules` body: `{"name","priority","script","failureMode"}`.

## Control API

`chilingd` serves `/v1` on the Unix socket and on localhost TCP.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/status` | Version, proxy address, counts |
| `GET` | `/v1/events` | Server-sent events |
| `GET` | `/v1/requests` | List. Query: `since`, `sinceTime`, `limit`, `method`, `host`, `path`, `status`, `q`, `tag`, `session` |
| `GET` | `/v1/requests/{id}` | Detail. `include=headers,body,rule-trace`. `body` is the request body, `responseBody` is the response body |
| `GET` `POST` | `/v1/sessions` | List or start a recording |
| `GET` | `/v1/sessions/current` | Active recording |
| `POST` | `/v1/sessions/{id}/stop` | Stop a recording |
| `GET` | `/v1/activity` | Trajectory (`session`, `limit`) |
| `POST` | `/v1/verify/replay` | Replay a capture |
| `POST` | `/v1/find` | Decision search. `{query, limit}` |
| `GET` | `/v1/decision` | Provider status, no secret |
| `POST` | `/v1/rules/draft` | Decision draft. `{description, requestId, apply}` |
| CRUD | `/v1/rules`, `/v1/fixtures`, `/v1/mitm/...` | Rules, fixtures, MITM |

Errors:

```json
{"error":{"code":"NOT_FOUND","message":"request not found","details":[{"field":"id","message":"..."}]}}
```

Events are best-effort. Slow subscribers can miss them. The request list is
the source for recent traffic. Bodies are previews (64 KiB each, 10 MiB
shared). Forwarding is not truncated to that cap. In-memory state is dropped
when the daemon exits.

## How a UI plugs in

No UI is built here. A native client (the planned mygo.dev app, with a
trajectory view along the lines of a harness timeline) should be a client of
this API, not a second proxy:

- `GET /v1/activity?session=` is the timeline: session start/stop, captures, rule creates, find, verify.
- `GET /v1/events` is the live tail.
- `GET /v1/requests` and `GET /v1/requests/{id}?include=headers,body,rule-trace` are the flow rows and the detail pane.
- `GET /v1/rules` plus enable, disable, and delete are the rule controls.
- `GET /v1/sessions` groups a recording the way a trajectory groups one run.

The UI should not apply rules by editing proxy state locally. It calls the
same routes the CLI calls.

## What the network design defers

`NETWORK_PIPELINE_DESIGN.md` is the data-plane target. This tree implements
the parts the agent loop needs:

- One exchange pipeline for plain HTTP and decrypted HTTP, with onion rule middleware.
- Immutable MITM policy snapshots. An empty allowlist denies interception.
- Bounded body capture that does not truncate the forwarded body.
- All-or-nothing rule validation, stable priority-then-id order, and a typed `abort` rejection.
- Shared upstream transport. `rewriteURL` changes dial target, TLS server name, and Host together.
- Owner-only Unix socket and a bearer token on TCP.

Still deferred, on purpose:

- Response-stage actions (`setResponseHeader`, body rewrite, delay, throttle).
- WebSocket frame inspection. Upgrades are not rewritten; opaque CONNECT still carries them.
- HTTP/2 MITM.
- SQLite history and retention.
- A plugin runtime.
- Full IDNA edge cases beyond `golang.org/x/net/idna` lookup, and a separate per-exchange memory budget beyond the capture caps.
- Automatic trust-store detection. `mitmCertInstalled` is still set by the CLI after install.

## Layout

| Path | Role |
|---|---|
| `cmd/chilingd` | Daemon |
| `cmd/chiling` | CLI |
| `internal/proxy` | Listener, exchange pipeline, CONNECT, replay |
| `internal/mitm` | CA and client-facing TLS |
| `internal/rule` | Goja sandbox, schema, snapshots |
| `internal/decision` | Jev and OpenAI-compatible decision calls |
| `internal/api` | `/v1` control API |
| `internal/session` | Recording sessions and trajectory |
| `internal/ring` | Recent flows |
