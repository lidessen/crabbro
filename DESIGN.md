# Chiling (敕令) Design

## Goal

Build a local network proxy for AI agents and human operators.

The app should behave like a developer proxy tool, but its primary interface is
agent-readable: agents must be able to inspect live traffic, fetch precise
request details, understand which rules changed a request, and generate or test
new rules through a CLI.

Human UI is still important, but it is a client of the same control plane, not
the owner of the proxy runtime.

## Non-Goals

- Mobile support.
- Cloud-hosted proxy control.
- Browser extension as the main product surface.
- Full Proxyman/Charles feature parity in the first release.
- Letting dynamic rules perform arbitrary filesystem, network, or process
  operations.

## Product Shape

The system has three surfaces:

1. `chilingd`: a Go daemon that owns proxying, capture, rule execution,
   storage, and the local control API.
2. `chiling`: a CLI designed for agents first, with JSON and NDJSON output
   as the default shape.
3. Desktop UI: a human-facing client for live requests, request details, rule
   browsing, and enable/disable controls.

The daemon is the source of truth. CLI and UI must not implement separate rule
semantics.

## Architecture

```text
                 ┌────────────────────┐
                 │     Desktop UI      │
                 │ SwiftUI or Tauri    │
                 └─────────┬──────────┘
                           │
                 ┌─────────▼──────────┐
                 │  Local Control API  │
                 │ UDS / localhost API │
                 └─────────┬──────────┘
                           │
┌──────────────┐   ┌───────▼────────┐   ┌────────────────┐
│ chiling  │   │  chilingd  │   │   Rule Store   │
│ CLI          ├──►│  Go daemon     ├──►│ JS + metadata  │
└──────────────┘   └───────┬────────┘   └────────────────┘
                           │
                 ┌─────────▼──────────┐
                 │ Proxy + Capture     │
                 │ HTTP/CONNECT/MITM   │
                 └─────────┬──────────┘
                           │
                 ┌─────────▼──────────┐
                 │ Event + History     │
                 │ ring buffer/SQLite  │
                 └────────────────────┘
```

## Daemon

`chilingd` is responsible for:

- Listening as a local HTTP proxy.
- Supporting HTTP proxy requests and HTTPS `CONNECT`.
- Capturing request and response metadata.
- Applying dynamic rules through Goja.
- Recording rule execution traces.
- Serving a local control API to CLI and UI.
- Managing optional HTTPS MITM certificates in later phases.

The daemon should be able to run without UI.

## Control API

Use a local-only control plane.

Preferred transport:

- Unix domain socket on macOS/Linux.
- Optional localhost TCP listener for easier UI integration and debugging.

The API should expose:

- Live event subscription.
- Request list and request detail queries.
- Rule list, create, update, enable, disable, and delete.
- Rule test against captured request fixtures.
- Daemon status and configuration.

The API should be versioned from the start, for example `/v1`.

## CLI

The CLI is agent-first. Commands should default to machine-readable output.

Examples:

```sh
chiling listen --json
chiling requests list --since 5m --json
chiling requests get <request-id> --include headers,body,rule-trace --json
chiling requests analyze <request-id> --json
chiling rules list --json
chiling rules enable <rule-id>
chiling rules disable <rule-id>
chiling rules generate --from-request <request-id> --intent "mock success"
chiling rules test ./rule.js --fixture <request-id> --json
```

Human-readable output can exist, but it should be explicit:

```sh
chiling requests list --pretty
chiling status --pretty
```

Live subscriptions should use NDJSON:

```json
{"type":"request_started","id":"req_123","method":"POST","url":"https://api.example.com/v1/chat"}
{"type":"rule_applied","requestId":"req_123","ruleId":"rule_mock_chat","actions":["mockResponse"]}
{"type":"response_completed","id":"req_123","status":200,"durationMs":42}
```

## Request Model

Every captured exchange should have a stable request id.

Minimum request fields:

- `id`
- `startedAt`
- `completedAt`
- `method`
- `scheme`
- `host`
- `path`
- `query`
- `url`
- `requestHeaders`
- `requestBodyRef`
- `status`
- `responseHeaders`
- `responseBodyRef`
- `durationMs`
- `clientAddr`
- `ruleTrace`
- `tags`

Body fields should use references instead of embedding large payloads directly
in list responses.

## Event Model

The daemon should emit structured events:

- `request_started`
- `request_headers`
- `request_body_available`
- `rule_evaluated`
- `rule_applied`
- `response_headers`
- `response_body_available`
- `response_completed`
- `request_failed`

Events are append-only observations. They should not be used as the only source
of persisted state once SQLite history is introduced.

## Rule System

Rules are written in JavaScript and executed by Goja.

Rules receive an immutable input snapshot and return a declarative decision.
They should not directly mutate Go request objects.

Example rule:

```js
export default function rule(ctx) {
  if (ctx.request.host !== "api.example.com") {
    return { match: false };
  }

  if (ctx.request.path === "/v1/chat") {
    return {
      match: true,
      actions: [
        {
          type: "mockResponse",
          status: 200,
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ ok: true })
        }
      ]
    };
  }

  return { match: false };
}
```

Rule outputs should be validated before application.

Supported initial actions:

- `tag`
- `block`
- `mockResponse`
- `setRequestHeader`
- `removeRequestHeader`
- `rewriteURL`

Later actions:

- `setResponseHeader`
- `removeResponseHeader`
- `rewriteResponseBody`
- `delay`
- `throttle`

## Rule Safety

The Goja runtime must be constrained:

- No filesystem access.
- No network access.
- No process spawning.
- No host environment access by default.
- Execution timeout per rule.
- Bounded input body preview.
- Bounded output size.
- Explicit schema validation for rule results.

Each rule evaluation should record:

- `ruleId`
- `ruleVersion`
- `matched`
- `actions`
- `durationMs`
- `error`

This trace is required so agents can explain what happened to a request.

## Storage

Use two layers:

1. In-memory ring buffer for live traffic and fast recent queries.
2. SQLite for durable request history, body references, rule versions, and rule
   traces.

The first implementation can start with only an in-memory ring buffer. SQLite
should be added once the event and request models are stable.

## UI Direction

For a macOS-only desktop app, prefer SwiftUI with narrow AppKit interop.

Reasons:

- Better fit for local proxy tools, menu bar behavior, system prompts, and
  certificate-related flows.
- Lower runtime overhead for a native desktop utility.
- The UI can remain a thin client over the daemon API.

Use Tauri if cross-platform desktop support becomes a first-class goal or if
the rule editor and table-heavy UX strongly benefit from a web stack.

## First Implementation Slice

Build the smallest useful agent-facing proxy:

1. Initialize a Go module.
2. Implement `chilingd` as a local HTTP proxy.
3. Support HTTP requests and HTTPS `CONNECT` passthrough.
4. Capture request metadata into an in-memory ring buffer.
5. Expose a local control API for:
   - daemon status
   - live events
   - request list
   - request detail
6. Implement `chiling` CLI commands:
   - `status`
   - `listen`
   - `requests list`
   - `requests get`
7. Add Goja rule execution for request-stage rules with:
   - `tag`
   - `block`
   - `mockResponse`
8. Record rule traces in request details.

This slice intentionally excludes HTTPS MITM, response body rewriting, SQLite,
and desktop UI.

## Later Milestones

### Milestone 2: Rule Testing

- Store captured requests as fixtures.
- Add `rules test`.
- Validate rule output schemas.
- Add rule execution timeout and output size limits.

### Milestone 3: HTTPS MITM

- Generate and install a local root certificate.
- Decrypt HTTPS traffic when enabled.
- Support domain allowlist and denylist.
- Make MITM opt-in and visible in status output.

### Milestone 4: Durable History

- Add SQLite.
- Persist request summaries, headers, body references, and rule traces.
- Add retention settings.

### Milestone 5: Human UI

- Show live request table.
- Show request/response detail panes.
- Show rule list.
- Enable, disable, and edit rules.
- Display rule traces for a selected request.

## Key Design Principles

- Agent-readable output is a product requirement, not an implementation detail.
- The daemon owns semantics; CLI and UI are clients.
- Rules return validated patches instead of mutating proxy internals directly.
- Request details must explain rule effects.
- Human UI should not delay the first useful agent-facing release.

## Decisions

The following decisions resolve open questions and inconsistencies in the design
above. They were made during the design review for the first implementation slice.

### Go Module Path

`github.com/lidessen/chiling`.

### Binary Layout

Two separate `main` packages in one Go module:
- `cmd/chilingd/` — the daemon binary.
- `cmd/chiling/` — the CLI client binary (communicates with the daemon over
  the control API).

### Default Ports and Paths

| Setting | Default | Configurable |
|---|---|---|
| Control API UDS | `~/.chiling/proxy.sock` | `--uds-path` |
| Control API TCP | `localhost:16379` | `--tcp-addr` |
| Proxy listener | `localhost:8080` | `--proxy-addr` |
| Ring buffer capacity | 1024 entries | `--ring-buffer-size` |
| Rule execution timeout | 500 ms | `--rule-timeout` |

### Control API Transport

Plain HTTP over Unix domain socket (primary) or localhost TCP (fallback). No
custom protocol, no gRPC.

### Control API Routes (v1)

All routes are under `/v1/`:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/status` | Daemon status and configuration |
| `GET` | `/v1/events` | Live event subscription (SSE) |
| `GET` | `/v1/requests` | Request list (pagination: `?since=`, `?limit=`, `?cursor=`) |
| `GET` | `/v1/requests/:id` | Request detail (`?include=headers,body,rule-trace`) |

### Error Response Format

Standard JSON error envelope:

```json
{
  "error": {
    "code": "NOT_FOUND",
    "message": "request not found",
    "details": [
      {"field": "id", "message": "invalid request ID format"}
    ]
  }
}
```

HTTP status codes: `400` (validation), `404` (not found), `500` (server error).

### Event Subscription Transport

Server-Sent Events (SSE) over HTTP. NDJSON event bodies map to SSE `data:` lines.

Events consistently reference requests by the field name `requestId`. The request
model's primary key is `id`.

### Rule Evaluation Semantics

- Rules are evaluated in ascending order of a numeric `priority` field (lower
  = first). If no priority is set, the rule is assigned priority 100.
- Every rule sees the same immutable request snapshot — rule chaining between
  rules is not supported.
- **All matching rules' actions are accumulated.** When multiple rules produce
  conflicting actions for the same dimension (e.g. two `mockResponse` actions),
  the **last matching rule wins**.
- Each evaluation emits a `rule_evaluated` event. If the rule matched and at
  least one action is taken, a `rule_applied` event is also emitted.

### Rule Identity and Storage (First Slice)

Rules are stored in an in-memory Go map keyed by rule ID. Rules are created via
the control API (`POST /v1/rules`) and are lost on daemon restart. No file-based
or database persistence in the first slice.

A rule has the following fields:
- `id` (string, generated if not provided)
- `name` (string, optional)
- `priority` (int, default 100)
- `enabled` (bool, default true)
- `script` (string, the JavaScript source)
- `createdAt`, `updatedAt` (timestamp)

### Body Storage (First Slice, No SQLite)

Bodies are stored in a separate in-memory LRU cache (not in the request ring
buffer):

- Max body size per request: 64 KB (truncated beyond that).
- Max total body cache: ~10 MB.
- Body references (`requestBodyRef`, `responseBodyRef`) point into this cache.
- If a body has been evicted, the reference resolves to null.

### In-Flight Requests

The request detail API returns whatever fields are available for in-flight
requests. `completedAt`, `status`, and response fields are omitted (not set to
null) until the exchange completes. The `id` is present once `request_started`
fires.

### CONNECT Tunnel Metadata

CONNECT requests produce a minimal request record:
- `method: "CONNECT"`, `host: "target:port"`, `path: ""`, `scheme: "https"`.
- `status`: 200 on successful tunnel establishment, error status otherwise.
- No request/response body. `durationMs` captures the tunnel lifetime.

### Revised First-Slice Actions

The first slice implements exactly three rule actions: `tag`, `block`,
`mockResponse`. The `setRequestHeader`, `removeRequestHeader`, and `rewriteURL`
actions are deferred to Milestone 2 to keep the first slice focused on the
agent-facing proxy loop.

### CLI `listen` Semantics

`chiling listen` subscribes to live events from a running daemon (SSE
stream). It does NOT start the daemon. Starting the daemon is always via
`chilingd`.

### Pagination

Cursor-based pagination for request lists. Responses include a `nextCursor`
field (absent when there are no more results). The `since` parameter accepts an
ISO 8601 timestamp or a Go duration string (e.g. `"5m"`, `"1h"`).

### Rule Trace

Each rule evaluation records:
- `ruleId`, `ruleVersion`, `matched`, `actions`, `durationMs`, `error` (if any).

This trace is attached to the request record and returned in request detail.
