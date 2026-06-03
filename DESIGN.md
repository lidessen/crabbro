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

Rule execution engine, fixture storage, rule CRUD, rules CLI commands, schema
validation, execution timeouts, and output size limits.

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

---

### Milestone 2 Decisions

The following decisions resolve the design for Milestone 2 (Rule Testing). These
extend the baseline established above.

#### Rule Engine (`internal/rule`)

A new Go package `internal/rule` provides Goja-based rule execution:

- **Isolated runtimes**: A new Goja VM is created per evaluation. No runtime is
  reused across evaluations or requests.
- **Sandbox**: The VM has no filesystem, no network, no process spawning, and no
  host environment access. Only ECMAScript 5.1 built-ins + `console.log` (no-op).
- **Input context**: Each rule receives a `ctx` object — `ctx.request` mirrors
  the Go `Request` struct (method, scheme, host, path, query, url, headers,
  body, clientAddr). The context is immutable from the rule's perspective.
- **Output contract**: Rules must return `{ match: boolean, actions?: Array<{ type, params }> }`.
  If `match` is false, actions are ignored.
- **Schema validation**: Every rule output is validated against a strict JSON
  schema after evaluation. See "Rule Output Validation Schema" below.
- **Timeout**: A 500 ms deadline (`DefaultRuleTimeout`) is enforced per rule via
  Goja's interrupt mechanism. Violations produce a timeout error in the trace
  and discard all actions from that rule.
- **Output size limits**: The serialized rule result (return value of the `default`
  function) is limited to 64 KB. `mockResponse.body` is additionally limited to
  64 KB. Exceeding these limits discards the rule's actions and records an error.

#### Actions

Milestone 2 supports all six rule actions. The last three were deferred from
Milestone 1 per the first-slice decisions.

| Action | Params | Description |
|---|---|---|
| `tag` | `{ tags: string[] }` | Adds tags to the request record |
| `block` | `{ status: number, body?: string }` | Aborts the request with the given status and optional body |
| `mockResponse` | `{ status: number, headers?: object, body?: string }` | Replaces upstream response with a synthetic one (max 64 KB body) |
| `setRequestHeader` | `{ name: string, value: string }` | Sets a request header before upstream forwarding |
| `removeRequestHeader` | `{ name: string }` | Removes a request header before upstream forwarding |
| `rewriteURL` | `{ url: string }` | Changes the upstream URL before forwarding |

#### Rule Output Validation Schema

The `default` function's return value must conform to this schema:

```jsonc
{
  "match": boolean,            // required
  "actions": [                  // optional; required when match=true
    {
      "type": enum,             // required: one of "tag", "block", "mockResponse",
                                //   "setRequestHeader", "removeRequestHeader", "rewriteURL"
      "params": { ... }         // required; per-type validation below
    }
  ]
}
```

Per-action `params` validation:

| Type | Field | Rule |
|---|---|---|
| `tag` | `params.tags` | required, must be non-empty `string[]` |
| `block` | `params.status` | required, must be integer 100–599 |
| `block` | `params.body` | optional, must be string |
| `mockResponse` | `params.status` | required, must be integer 100–599 |
| `mockResponse` | `params.headers` | optional, must be object with string values |
| `mockResponse` | `params.body` | optional, must be string, max 64 KB |
| `setRequestHeader` | `params.name` | required, must be non-empty string |
| `setRequestHeader` | `params.value` | required, must be string |
| `removeRequestHeader` | `params.name` | required, must be non-empty string |
| `rewriteURL` | `params.url` | required, must be valid HTTP(S) URL string |

#### Rule Storage (Daemon Side)

Rules are stored in an in-memory `map[RuleID]*Rule` (no file or database
persistence in Milestone 2 — lost on daemon restart). Extended from the
first-slice design with full CRUD support:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/rules` | List all rules |
| `POST` | `/v1/rules` | Create a rule (`{ name, priority, script }`) |
| `GET` | `/v1/rules/:id` | Get a single rule |
| `PUT` | `/v1/rules/:id` | Update rule fields |
| `DELETE` | `/v1/rules/:id` | Delete a rule |
| `POST` | `/v1/rules/:id/toggle` | Enable or disable a rule |
| `POST` | `/v1/rules/test` | Test a rule script against a fixture |

#### Rule Test Endpoint

`POST /v1/rules/test` accepts:

```jsonc
{
  "ruleScript": "export default function(ctx) { ... }",  // required
  "fixture": "fixture-name",      // one of these three
  "fixtureId": "req_abc",         //   must be set
  "fixtureFile": "path/to/fixture.json"  //
}
```

Response:

```jsonc
{
  "matched": true,
  "actions": [{ "type": "mockResponse", "params": { "status": 200 } }],
  "durationMs": 3,
  "schemaValid": true,
  "schemaErrors": null,
  "error": null,
  "trace": {
    "ruleId": "",
    "ruleVersion": 0,
    "matched": true,
    "actions": [{ "type": "mockResponse", "params": { "status": 200 } }],
    "durationMs": 3,
    "error": ""
  }
}
```

#### Fixture Storage

Fixtures are saved as JSON files under `~/.chiling/fixtures/<name>.json`:

```jsonc
{
  "name": "example-api-chat",
  "createdAt": "2026-06-03T10:00:00Z",
  "request": {
    "method": "POST",
    "scheme": "https",
    "host": "api.example.com",
    "path": "/v1/chat",
    "query": "",
    "url": "https://api.example.com/v1/chat",
    "requestHeaders": {"content-type": "application/json"},
    "requestBody": "{\"prompt\":\"hello\"}",
    "responseHeaders": {"content-type": "application/json"},
    "responseBody": "{\"reply\":\"hi\"}",
    "status": 200,
    "clientAddr": "127.0.0.1:54321"
  }
}
```

Body data is **inlined** (the BodyCache references are resolved at save time)
so fixtures are self-contained and survive daemon restarts.

Fixture API:

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/fixtures` | Save a captured request as a fixture (`{ requestId, name }`) |
| `GET` | `/v1/fixtures` | List saved fixtures |
| `GET` | `/v1/fixtures/:name` | Get fixture detail (the full JSON) |
| `DELETE` | `/v1/fixtures/:name` | Delete a fixture file |

#### CLI Extensions

New commands under the existing `chiling` binary:

```
chiling rules list [--json|--pretty]
chiling rules create <file> [--name <name>] [--priority <n>]
chiling rules get <id> [--json|--pretty]
chiling rules enable <id>
chiling rules disable <id>
chiling rules delete <id>
chiling rules test <rule-file> --fixture <name> [--json|--pretty]
chiling rules fixture save <request-id> <name>
chiling rules fixture list [--json|--pretty]
chiling rules fixture delete <name>
```

The `rules test` command reads the JS file from disk, resolves the fixture
reference via the daemon API, submits both to `POST /v1/rules/test`, and
displays the result (JSON by default, pretty-printed with `--pretty`).

#### Rule Evaluation in the Proxy Pipeline

When a request arrives at the proxy:

1. After creating the request record and reading the body, evaluate all
   **enabled** rules in ascending priority order.
2. Each rule receives an immutable snapshot of the request.
3. All matching rules' actions are **accumulated** into a list.
4. Conflicting actions for the same dimension (e.g., two `mockResponse`
   results) are resolved by **last-matching-rule wins**.
5. Action application order:
   - `setRequestHeader` / `removeRequestHeader` mutate the upstream request.
   - `rewriteURL` replaces the upstream URL.
   - `block` / `mockResponse` short-circuit the upstream call (block wins if both present).
   - `tag` updates the request record (union of all tag action tags).
6. Every evaluation appends a `RuleTrace` to the request's `RuleTrace` slice.
7. Relevant events (`rule_evaluated`, `rule_applied`) are emitted via EventHub.

#### Testing Approach

- **Rule engine unit tests**: Test Goja execution with valid scripts, schema
  violations, script errors, timeout enforcement, and output size limits.
- **API integration tests**: Test rule CRUD endpoints, fixture save/list/get/delete,
  and the `rules/test` endpoint against mock fixtures.
- **CLI smoke**: Run `chiling rules test` against a running daemon with a fixture.
- **Goja sandbox verification**: Explicit tests that FS/network/process calls in
  rule scripts are blocked.

## Additional Events (Second Slice)

The following event types are added in Milestone 3 for daemon-scoped configuration
changes. Unlike request-scoped events, they carry no `requestId`.

| Event Type | Payload | Description |
|---|---|---|
| `mitm_enabled` | `{}` | Emitted when MITM is enabled |
| `mitm_disabled` | `{}` | Emitted when MITM is disabled |

---

### Milestone 3: HTTPS MITM

The following decisions extend the design for Milestone 3 (HTTPS MITM). These
address certificate generation and lifecycle, TLS interception, domain filtering,
configuration, and the control API surface.

#### Pre-Mortem — Why the Selected Approach Could Fail

The preferred approach is **built-in Go `crypto/tls`** for all TLS operations
(CA generation, host cert generation, client-side and server-side TLS). Before
detailing the decisions, the pre-mortem for this approach:

- **Certificate chain issues**: Some clients or servers may reject generated certs
  if the chain is incomplete or the root CA is not in the system trust store.
  *Mitigation:* Always emit a complete chain, document `install-ca` as a required
  step, test against common clients (curl, browsers, Go HTTP client).
- **Performance**: Generating an RSA 2048-bit leaf cert per host on first
  connection introduces ~50–200ms latency. *Mitigation:* LRU in-memory cache
  (max 1000 entries) so each host pays this cost once per daemon lifetime.
- **TLS version mismatch**: Go's default TLS 1.2/1.3 config may not match what
  every upstream expects. *Mitigation:* Use Go's defaults (TLS 1.2+), which are
  broadly compatible. Document known-incompatible origins.
- **Memory growth**: Cert cache and per-connection buffers could accumulate.
  *Mitigation:* LRU eviction at 1000 entries, bounded TLS record buffers.
- **Platform trust-store variance**: Installing a root CA requires different
  commands per OS. *Mitigation:* Support macOS and Linux in the MVP; document
  the manual install path for other platforms.

#### Package Layout

A new Go package `internal/mitm` with:

| File | Responsibility |
|---|---|
| `ca.go` | Root CA generation (`GenerateCA`), PEM serialization, file load/save |
| `certgen.go` | Host certificate generation (`GenerateHostCert`), LRU cache (`CertManager`) |
| `intercept.go` | TLS interception handler — the MITM proxy loop |
| `config.go` | Allowlist/denylist types with pattern matching (`DomainMatcher`) |

#### Certificate Authority

- Root CA generated using Go's `crypto/rand`, `crypto/rsa` (2048-bit), and
  `crypto/x509` — **no external dependencies**.
- CA certificate stored as PEM at `~/.chiling/certs/rootCA.pem`.
- CA private key stored as PEM at `~/.chiling/certs/rootCA-key.pem`.
- CA is generated **once** on first `--mitm` launch. On subsequent starts the
  existing files are loaded (the key is reused so host certs signed in a prior
  session are still valid if cached — though in MVP the host cert cache is
  in-memory only, so this is for future persistence).
- Cert directory `~/.chiling/certs/` created with `0700` on first start.
- CA cert attributes:
  - Validity: 10 years from generation.
  - `BasicConstraints`: `CA: true`.
  - `KeyUsage`: `CertSign`, `DigitalSignature`.
  - Subject `CN`: `"chiling root CA"`.
  - `IsCA`: `true`.

#### Per-Host Certificate Generation

- On first MITM connection to a new host, a leaf cert is generated on demand
  signed by the root CA.
- Leaf cert attributes:
  - `Subject CN`: the target hostname (e.g. `api.example.com`).
  - `DNSNames` SAN: `[hostname]` (exactly one entry).
  - Key: ephemeral 2048-bit RSA (generated fresh per leaf).
  - Signature: SHA-256 with RSA.
  - Validity: 1 year from generation.
- Leaf certs are cached in an **in-memory LRU map** (max 1000 entries, protected
  by `sync.RWMutex`).
- Cache is **not persisted** to disk in this milestone — lost on daemon restart.
- Cache key: hostname string (lowercased). Collisions are impossible since each
  hostname maps to exactly one cert.

#### Certificate Installation (CLI)

The `chiling mitm install-ca` command copies the root CA into the OS trust store.

- **macOS**: `security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.chiling/certs/rootCA.pem`
- **Linux**: copy to `/usr/local/share/ca-certificates/chiling-root.crt` then
  `update-ca-certificates`
- **Behavior**: The command prints the exact command(s) it will run, then runs
  them with `sudo` if needed. Failure is reported (non-fatal — the user can
  install manually).
- **Tracking**: The daemon exposes a `mitmCertInstalled` boolean in status. In
  the MVP this is set by the CLI after a successful install (not auto-detected).

#### MITM TLS Interception Flow

The existing `handleConnect` in `internal/proxy` is extended with a branch:

1. CONNECT request arrives for `host:port`.
2. MITM gate check: is MITM enabled? Does `host` pass allowlist/denylist?
3. **If MITM does not apply**: fall through to the existing TCP tunnel behavior
   (unchanged passthrough).
4. **If MITM applies**:

   a. **Upstream connection**: Dial `host:port` via TCP, then perform a TLS
      client handshake (the proxy acts as a TLS client to the real server).
      The TLS config uses Go's defaults with `ServerName` set to the hostname.

   b. **Client handshake**: Retrieve (or generate) a host-specific cert from
      the `CertManager`. Perform a TLS server handshake with the local client,
      presenting the host cert. Now the proxy has two `*tls.Conn` connections:
      one decrypted (client side) and one encrypted (upstream side).

   c. **Request loop**: Read HTTP/1.1 requests from the decrypted client
      connection using Go's `http.ReadRequest` in a loop (`bufio.Reader`).

   d. For each decrypted HTTP request:

      i. **Create a request record** with `Method`, `Scheme: "https"`,
         `Host`, `Path`, `Query`, `URL` all populated from the decrypted
         request. `Intercepted: true` is set on the record.

      ii. **Capture and cache** the request body (same BodyCache as HTTP path).

      iii. **Evaluate rules** against the decrypted request (same
           `evaluateRules` path as HTTP handler).

      iv. **Forward** the request to the upstream TLS connection, reading
          headers, handling rule-applied header modifications, and sending
          the body.

      v. **Read the upstream response** from the upstream TLS connection.

      vi. **Capture response** metadata and body, emit events.

      vii. **Write the response** back to the client over the decrypted
           TLS connection.

      viii. **Complete the request record** (set `CompletedAt`, `DurationMs`,
            `Status`, emit `response_completed`).

   e. On client disconnect, stream error, or timeout: close both TLS
      connections and exit the loop.

5. **Request records created** during interception:
   - A CONNECT record is still created for the tunnel itself (marked
     `Intercepted: true`).
   - Each HTTP exchange within the tunnel gets its own request record (also
     `Intercepted: true`).

#### HTTP Version Handling

- **HTTP/1.1 only** in this milestone. The request loop handles one request at
  a time (no pipelining — each request is fully processed before reading the
  next). `Connection: keep-alive` is respected for the client connection.
- **HTTP/2** is deferred. If the client attempts HTTP/2 via ALPN, the proxy
  cannot inspect it. The proxy's server-side TLS config does not advertise
  HTTP/2 ALPN, so the client falls back to HTTP/1.1. Upstream HTTP/2 responses
  are accepted and forwarded as HTTP/1.1 to the client (Go's TLS client
  negotiates HTTP/2 automatically with the upstream; the response bytes are
  streamed back through the HTTP/1.1 connection).

#### Domain Allowlist and Denylist

Two independent lists of domain patterns:

```go
type DomainMatcher struct {
    mu       sync.RWMutex
    patterns []string // each may contain * wildcards
}
```

- **Pattern format**: Each pattern is a domain string. `*` matches any sequence
  of characters except `.`. So `*.example.com` matches `a.example.com` but not
  `a.b.example.com`. A bare `*` matches all domains.
- **Denylist takes precedence**: If a host matches the denylist, MITM is
  skipped regardless of the allowlist.
- **Empty allowlist** (nil/empty slice) = MITM permitted for all domains
  (subject to denylist).
- **Non-empty allowlist** = MITM only for matching domains.
- Matching is case-insensitive. The host part only (port is stripped).
- Pattern lists are mutable at runtime via the control API.

#### Configuration and Lifecycle

- MITM is **disabled by default** (opt-in).
- **Startup flags** on `chilingd`:
  - `--mitm`: Enable MITM on startup.
  - `--mitm-allow <pattern>`: Add an allowlist entry (repeatable).
  - `--mitm-deny <pattern>`: Add a denylist entry (repeatable).
- **Runtime control** via the control API (see routes below). Toggling MITM
  at runtime affects new connections only; existing intercepted tunnels
  continue until the client disconnects.

#### Control API Routes

New routes under `/v1/mitm/`:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/mitm` | Return MITM status: `{ enabled, certInstalled, allowCount, denyCount }` |
| `POST` | `/v1/mitm/enable` | Enable MITM (body: `{}`). Emits `mitm_enabled` event. |
| `POST` | `/v1/mitm/disable` | Disable MITM. Emits `mitm_disabled` event. |
| `GET` | `/v1/mitm/ca` | Return root CA PEM as `application/x-pem-file` |
| `POST` | `/v1/mitm/allow` | Add to allowlist: `{ "pattern": "*.example.com" }` |
| `POST` | `/v1/mitm/deny` | Add to denylist: `{ "pattern": "*.example.com" }` |
| `DELETE` | `/v1/mitm/allow` | Remove from allowlist: `{ "pattern": "*.example.com" }` |
| `DELETE` | `/v1/mitm/deny` | Remove from denylist: `{ "pattern": "*.example.com" }` |

#### Status Response Extension

`/v1/status` response gains two fields:

```jsonc
{
  "mitmEnabled": false,
  "mitmCertInstalled": false,
  // … existing fields unchanged
}
```

#### Request Model Extension

A new boolean field on `shared.Request`:

```go
// Intercepted is true when the request was MITM-decrypted (as opposed to a
// raw TCP CONNECT tunnel passthrough).
Intercepted bool `json:"intercepted,omitempty"`
```

#### CLI Commands

```text
chiling mitm status [--json|--pretty]
chiling mitm enable
chiling mitm disable
chiling mitm install-ca
chiling mitm allow <pattern>
chiling mitm deny <pattern>
chiling mitm allow rm <pattern>
chiling mitm deny rm <pattern>
```

All MITM commands communicate with the daemon's control API (same as `status`,
`requests`, `rules`).

#### Testing Approach

| Layer | Scope |
|---|---|
| **CA unit tests** | `GenerateCA` produces a valid cert+key pair; cert is self-signed; PEM round-trip |
| **Cert generation tests** | `GenerateHostCert` produces a cert valid for the given hostname; signature verifies against CA |
| **Domain matcher tests** | Allowlist/denylist matching with exact, wildcard, and empty patterns |
| **MITM integration test** | Start daemon with MITM, connect via HTTPS (using the generated CA), verify decrypted request is captured, bodies are readable |
| **Rule interaction test** | Rule (e.g., `mockResponse`) applied to a decrypted HTTPS request returns the mock, not the real upstream response |
| **Allow/deny test** | Domains in denylist pass through as opaque tunnels; allowlist correctly restricts MITM scope |

#### Deferred (Future Milestones)

| Feature | Reason |
|---|---|
| HTTP/2 MITM | Requires HTTP/2 frame parsing or Go's `golang.org/x/net/http2`; keep MVP on HTTP/1.1 only |
| Response-stage rule evaluation | Intercepted exchanges still only run request-stage rules; response rules added when the rule engine supports response-stage evaluation |
| Persistent host cert cache | Regenerating on restart is fast enough for MVP; persisted cache adds complexity of disk format and staleness checks |
| Automatic CA rotation | 10-year validity makes this low urgency for a dev tool |
| OCSP stapling | Adds significant complexity for unlikely-before-2030 benefit in a local proxy |
| Certificate transparency bypass | Not needed for developer proxy use cases |

---

### Milestone 4: MVP Connectivity (Setup, Doctor, Proxy Wiring)

The following decisions close the gap between the implemented MITM infrastructure and a
working end-to-end operator experience. They were identified during the MVP design
review (task_4babbed2-cb2e-4193-9ce2-d62c1c8b6dba).

#### Pre-Mortem — Why This Plan Could Fail

The preferred approach is **direct field assignment** for MITM proxy wiring and
**OS-native command execution** for CA install/uninstall. Pre-mortem:

- **Security authorization denied**: `security add-trusted-cert` may require
  Touch ID / admin password and the operator may refuse. *Mitigation:* Always
  show the exact OS command before execution; if authorization fails, print
  the manual command the user can run themselves.
- **Platform variance**: The macOS `security` tool CLI flags could change across
  OS versions. *Mitigation:* Target the current macOS Sequoia command set; test
  against the pinned OS version; fall back to printing instructions on failure.
- **Daemon not running**: The CLI's `install-ca` command needs to inform the
  daemon that the cert is installed, but the daemon may not be running.
  *Mitigation:* The install succeeds independently; the daemon flag is a
  best-effort API call. The `doctor` command re-checks and can resync.
- **Multiple cert installs**: Running `install-ca` twice should not duplicate or
  error. *Mitigation:* `security add-trusted-cert` is idempotent (updates the
  existing entry). Detect already-installed state and report it.
- **Uninstall removes wrong cert**: If the cert file path used at uninstall time
  differs from the one installed, removal could fail silently. *Mitigation:*
  Always use the same `~/.chiling/certs/rootCA.pem` path; verify that the
  certificate being removed matches the stored PEM fingerprint.

#### Decision: MITM Proxy Wiring

**Approach: Direct public-field assignment in `chilingd/main.go`**

- Add **two lines** after `proxy.New()` in `chilingd/main.go`:
  ```go
  pxy.MITMHandler = mitmHandler
  pxy.MITMEnabled = *mitmEnabled
  ```
- The `Proxy.MITMHandler` and `Proxy.MITMEnabled` fields already exist and are
  already checked in `handleConnect` (`connect.go`). The test code
  (`testMITMSetup`) already does exactly this.
- No changes to `proxy.New()` signature, no new setter methods.
- **MitMHandler must be non-nil** when MITM is used at runtime. Since the handler
  is nil when neither `--mitm` nor `--mitm-allow`/`--mitm-deny` is set, `install-ca`
  must also create/load the CA (the `chiling mitm install-ca` CLI command will
  ensure the CA files exist).

| Pros | Why alternatives rejected |
|---|---|
| One-line change per field | Extending `proxy.New()` signature breaks all callers and adds ceremony |
| Matches pattern used in tests | Setter methods add abstraction without benefit for public fields |
| No new API surface | The proxy struct fields are already public and documented |

#### Decision: CA Install / Uninstall

**Approach: OS-native command execution with explicit consent**

- **Install**: `chiling mitm install-ca`
  1. Verify CA file exists at `~/.chiling/certs/rootCA.pem`. If not, download it
     from the daemon API (`GET /v1/mitm/cert`) or print instructions to generate
     it by starting the daemon with `--mitm`.
  2. Print the exact command(s) that will be executed and a warning about
     modifying the system trust store.
  3. Ask the user for explicit confirmation (Y/n prompt).
  4. Execute the OS-specific command:
     - **macOS**: `security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.chiling/certs/rootCA.pem`
     - **Linux**: `sudo cp ~/.chiling/certs/rootCA.pem /usr/local/share/ca-certificates/chiling-root.crt && sudo update-ca-certificates`
  5. On success, call `POST /v1/mitm/cert-installed` on the daemon API (best-effort)
     to update the `mitmCertInstalled` status flag.
  6. On failure, print the manual command the user can run.

- **Uninstall**: `chiling mitm uninstall-ca`
  1. Print the command(s) that will be executed and a warning.
  2. Ask for explicit confirmation.
  3. Execute:
     - **macOS**: `security remove-trusted-cert -d ~/.chiling/certs/rootCA.pem`
     - **Linux**: `sudo rm /usr/local/share/ca-certificates/chiling-root.crt && sudo update-ca-certificates`
  4. On success, call `POST /v1/mitm/cert-installed` with `false` (best-effort).

- **API extension**: Add a new API endpoint:
  - `POST /v1/mitm/cert-installed` with body `{ "installed": bool }`
  - The daemon sets the `mitmCertInstalled` flag (persisted in-memory only in MVP)
  - This allows the CLI to update the daemon's status without the daemon having
    to detect trust store state itself

| Pros | Why alternatives rejected |
|---|---|
| Zero-touch for common case | Manual-only mode (Option B) adds friction; for a developer tool the automated flow is expected |
| Transparent — shows every command | Desktop-native dialogs (Option C) vary by platform and add scriptable auth complexity |
| Best-effort daemon sync — doesn't require daemon to be running | |
| Cross-platform with fallback | |

#### Decision: Uno Doctor Command

**Approach: `chiling doctor` — diagnostic and status overview**

A single command an operator runs to check if the system is set up correctly:

```
chiling doctor [--fix]
```

Output (JSON by default, `--pretty` for human-readable):

```jsonc
{
  "daemonRunning": true,
  "proxyListening": true,
  "proxyAddr": "localhost:8080",
  "mitmConfigured": true,
  "mitmEnabled": false,
  "mitmCertExists": true,
  "mitmCertInstalled": false,
  "rulesCount": 3,
  "requestsCaptured": 42,
  "issues": [
    "MITM CA certificate is not installed in the system trust store. Run 'chiling mitm install-ca' to install it."
  ]
}
```

- **Detects daemon status**: Try UDS, fall back to TCP. If unreachable, report
  and show `chilingd` start command.
- **Checks CA file existence**: Verify `~/.chiling/certs/rootCA.pem` exists.
- **Checks CA install status**: Query daemon via `GET /v1/status`. If daemon is
  unreachable, check trust store directly (macOS: `security find-certificate -c "chiling root CA" -p`).
- **`--fix` flag**: Attempts auto-remediation for fixable issues (start daemon,
  install CA, enable MITM). Each fix step shows the action and asks confirmation.
- **JSON output**: Agent-consumable. Use `--pretty` for human operators.

| Pros | Why alternatives rejected |
|---|---|
| Single entry point for troubleshooting | Integrating into `chiling status --pretty` (Option B) overloads the status command |
| Agent-friendly JSON + human pretty | Skipping entirely (Option C) leaves operators without diagnostic tools |
| `--fix` can automate common issues | |

#### Decision: Updated CLI Help

The usage text in `cmd/chiling/main.go` is updated to include the new commands:

```text
  mitm install-ca             Install the root CA into the system trust store
  mitm uninstall-ca           Remove the root CA from the system trust store
  doctor                      Check system setup and diagnose issues
    --fix                       Attempt to fix detected issues
```

#### API Changes

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/mitm/cert-installed` | Notify daemon that CA cert was installed/uninstalled. Body: `{ "installed": bool }` |

No other API changes. The existing `GET /v1/mitm/cert` already serves the CA PEM.

#### Test Plan

| Layer | Scope |
|---|---|
| **Wiring test** | Unit test verifying that setting `Proxy.MITMHandler` + `Proxy.MITMEnabled` in `chilingd/main.go` produces a proxy that intercepts CONNECT requests |
| **CA install unit** | `cmdMITMInstallCA` calls the correct OS command; handles already-installed state; error response formatting |
| **CA uninstall unit** | `cmdMITMUninstallCA` calls the correct OS command; handles already-removed state |
| **Doctor unit** | Test daemon-reachable and daemon-unreachable paths; test with and without MITM; test `--fix` flag |
| **Doctor integration** | Start daemon, run doctor, verify JSON output matches daemon state |
| **API cert-installed** | Test `POST /v1/mitm/cert-installed` sets/clears the flag |
