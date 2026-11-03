# Chiling Network Data Plane and Middleware Design

Status: Proposed design, not implemented
Date: 2026-11-03
Scope: Complete the HTTP/CONNECT/MITM network path and establish one synchronous middleware model for current dynamic rules and future trusted extensions.

## 1. Decision Summary

1. Treat HTTP proxying, CONNECT routing, TLS interception, HTTP exchange processing, upstream transport, capture, and control as separate responsibilities.
2. Route ordinary HTTP requests and HTTP requests decrypted inside MITM through the same Exchange pipeline.
3. Compose synchronous middleware as an onion: request processing runs from outer to inner; response processing unwinds from inner to outer.
4. Build HTTP parsing and response framing on Go's `net/http` server and transport. Keep raw socket handling at the CONNECT tunnel and protocol-upgrade boundaries.
5. Define hooks as typed adapters over middleware. Keep hooks synchronous and ordered. Keep EventHub events asynchronous and observational; events never control forwarding.
6. Compile validated rules and network policy into immutable snapshots, then publish each snapshot atomically. A request uses one rule snapshot for its full lifetime.
7. Keep plugin installation, plugin manifests, arbitrary host capabilities, and command registration out of the current implementation.
8. Make body inspection bounded and non-destructive. Keep unchanged request and response bodies streaming through the proxy.

This proposal supersedes the earlier network implementation choices in DESIGN.md where they conflict, especially direct mutable MITM flags, duplicate plain HTTP/MITM handlers, and rule-result accumulation outside a shared exchange pipeline.

## 2. Goals and Non-Goals

### Goals

- Correctly forward HTTP/1.1 proxy requests, including request bodies, query strings, headers, trailers, and connection semantics.
- Support opaque CONNECT tunnels for destinations outside the MITM policy.
- Support HTTP/1.1 requests inside explicitly intercepted CONNECT tunnels.
- Use one Exchange pipeline for ordinary HTTP and decrypted HTTP.
- Add request hooks with explicit short-circuit outcomes and response hooks with validated patches, deterministic order, trace records, and bounded body access.
- Stream bodies when no middleware needs to transform them.
- Honor client cancellation, upstream timeouts, active connection tracking, and graceful shutdown.
- Preserve the current rule runtime’s sandbox, execution timeout, output limit, and schema validation.
- Keep agent-readable request records and rule traces.

### Design Defaults

- Preserve the current body-capture-on behavior with a 64 KiB per-body cap and 10 MiB shared body-cache cap as initial defaults, but enforce the cap while bytes flow. Make both configurable and provide metadata-only mode.
- Keep the current 500 ms per-rule execution limit and add a separate total evaluation budget per Exchange.
- Default rule evaluation and oversized body-transform failures to fail-open. A rule must explicitly select fail-closed and define the client response it wants when enforcement cannot complete.
- Default an empty MITM allowlist to deny interception. This intentionally changes the older documented behavior and must be called out in the control API/CLI.

### Non-Goals for This Design

- A plugin loader, plugin registry, downloaded plugin support, or plugin command framework.
- Arbitrary filesystem, network, environment, or process capabilities for JavaScript rules.
- MITM support for HTTP/2. Opaque CONNECT passthrough continues to carry any TLS protocol.
- WebSocket frame inspection or transformation in the first network implementation. Intercepted WebSocket upgrades must still be relayed transparently.
- Durable flow storage or SQLite migration.
- Full Charles/Proxyman feature parity.

## 3. Current State and Design Implications

The current code has separate forwarding implementations in internal/proxy/handler.go and internal/proxy/connect.go. The ordinary HTTP path reads and consumes the client body, then creates an upstream request without restoring that body. Both paths read complete request and response bodies before forwarding. The body cache truncates stored bytes after those reads, so its limit does not bound network-side memory use.

The MITM path uses a second HTTP implementation around http.ReadRequest and a raw TLS dial. It computes a rewritten URL but still dials the original CONNECT host. It has no transparent duplex relay after a WebSocket 101 response.

MITM runtime state is split between Proxy.MITMEnabled and MITMHandler.Enabled. The API updates the latter while CONNECT routing checks the former. The earlier public-field wiring decision in DESIGN.md creates two sources of truth and is replaced by one immutable network-policy snapshot.

Rule evaluation currently runs outside a common request/response lifecycle. `EnabledRules` returns pointers to mutable store entries and sorts them for each request. Equal-priority ordering is not stable. Request header set/remove actions are accumulated separately, so their actual resolution does not always match the documented last-rule-wins behavior. The existing design also says all rules see the same request snapshot; the proposed onion deliberately changes that contract so inner rules see outer request patches.

The MITM request loop uses `http.ReadRequest` and a custom response writer, duplicating HTTP lifecycle behavior. The control API's default TCP listener is localhost-only but has no authentication, and its Unix socket is currently chmodded to `0666`; both are too broad for an API that can expose captured bodies and change proxy policy across local users.

Crabbro demonstrates useful mechanisms, but not a complete target architecture:

| Crabbro mechanism | Keep or change in chiling |
|---|---|
| Rules are loaded into an ordered `RuleSet` and published atomically; file changes can hot-reload the set. | Keep immutable snapshot publication. Compile and validate the whole candidate before swapping it; a bad candidate must not partially replace the active set. |
| Middleware has a stable name and priority, and rules can modify both request and response. | Keep stable identity, priority, and declarative request/response actions. Replace its separate forward loops with onion composition so response order is structurally the reverse of request order. |
| MITM host matching can be widened by hosts referenced in rules. | Keep MITM scope in explicit network policy. A rule match must not silently grant permission to decrypt a host. |
| WebSocket paths are treated as a distinct duplex lifecycle. | Keep the explicit protocol boundary. Initially relay intercepted upgrades transparently; frame-level rules remain deferred. |
| Its default proxy listener is `0.0.0.0:9090`, which is useful for LAN device capture. | Chiling defaults to loopback for a local agent proxy. Any future LAN listener should be explicit and authenticated. |
| The proxy is built on goproxy, and rule/plugin scripts can receive broad native host capabilities. | Do not inherit these coupling and capability choices. Use standard-library HTTP lifecycle primitives and retain chiling's declarative, restricted Goja rule boundary. |

This comparison is based on the current source in both repositories. It is not a claim that either repository's architecture is the design to preserve.

## 4. Ownership Model

| Component | Owns |
|---|---|
| Proxy listener | Accepting client HTTP proxy requests and CONNECT requests; listener and server shutdown |
| CONNECT router | Validating authority, selecting passthrough or MITM from one policy snapshot, tracking tunnel lifetime |
| Plain HTTP ingress adapter | Normalize an absolute-form proxy request into an Exchange and later write the returned response to the client |
| MITM HTTP ingress adapter | Build an HTTPS URL from the decrypted request target and parent CONNECT authority, then create the same Exchange shape |
| MITM adapter | Client-facing TLS handshake and generated leaf certificate; feed the decrypted connection to the standard HTTP parser and MITM ingress adapter |
| Exchange | One logical HTTP request/response, its original and effective request, decision, trace, and timing |
| Middleware chain and hook adapters | Ordered synchronous request and response behavior, with request-local state |
| Upstream transport | DNS/dial, TLS verification, connection reuse, RoundTrip, cancellation, and upstream errors |
| Body access and capture | Non-destructive previews, bounded transforms, capture references, truncation state |
| Rule runtime | Evaluate a bounded immutable request/response view and return a validated patch or decision |
| Event/history layer | Observe lifecycle facts and expose recent request records; it does not steer network execution |
| Control API | Publish rule and MITM-policy snapshots; it does not own in-flight exchange state |

The Exchange is the data-plane unit. A CONNECT tunnel can contain many Exchanges. The tunnel has a stable ID; each decrypted HTTP exchange has its own request ID and a parent tunnel ID.

An Exchange holds the request context, immutable original request view, effective outbound request, optional response, selected rule snapshot, snapshot generations, per-hook state, body-access state, trace, and timing. It does not own an unbounded copy of either body. The history record is a projection finalized from the Exchange; it is not the mutable object passed to rule scripts.

    Parsed -> Request hooks -> Upstream or synthetic response -> Response hooks -> Downstream copy -> Completed
                                   |                                 |                     |
                                   +----------- error/cancel --------+---------------------+-> Failed/Cancelled

## 5. Target Architecture

    Client
      └─ Proxy listener
          ├─ Absolute-form HTTP request ──────────────┐
          └─ CONNECT                                 │
              ├─ Passthrough: opaque bidirectional relay
              └─ MITM: TLS adapter -> net/http server ┘
                                                       ↓
                       Normalize request -> Exchange
                                                       ↓
                        Onion middleware pipeline
                      request inward / response outward
                                                       ↓
                          Shared RoundTripper
                                                       ↓
                      Stream response to the client
                                                       ↓
                       Capture and finalize record

CONNECT passthrough does not enter the HTTP middleware chain. Each HTTP request inside an intercepted TLS connection does.

### HTTP Lifecycle Boundary

Use `http.Server` for both ordinary proxy HTTP requests and decrypted HTTP/1.1 requests. The ordinary proxy ingress routes CONNECT and normalizes absolute-form HTTP; the MITM ingress uses its parent tunnel authority to normalize origin-form HTTP. Both call the same Exchange handler. For MITM, complete the client-facing TLS handshake with the certificate selected for the CONNECT authority, then serve that already-established TLS connection through a one-connection listener. This keeps Go's request parsing, keep-alive, body framing, `Expect: 100-continue`, cancellation, and response framing in one implementation. Do not maintain a second hand-written `http.ReadRequest` loop.

Use a shared `http.Transport` as the terminal upstream path. Middleware returns or wraps an `http.Response`; a single downstream adapter writes it. Raw bidirectional copying is reserved for CONNECT passthrough and protocol upgrades such as WebSocket.

### Package Responsibilities

- internal/proxy owns listener routing, HTTP ingress adaptation, Exchange, pipeline composition, CONNECT lifecycle, and downstream response writing.
- internal/mitm owns CA and leaf certificate operations plus the client-facing TLS handshake adapter. It does not parse HTTP requests, own rule execution, or forward upstream HTTP requests.
- internal/rule owns rule compilation, request matching, timeout enforcement, and validated outcomes. It does not mutate net/http request objects directly.
- internal/shared owns transport-independent request, event, trace, and API data types.
- The upstream transport is a shared net/http RoundTripper with explicit settings. It is not constructed independently for every request.

These are ownership boundaries, not a requirement to create one package per row immediately.

## 6. Protocol Flows

### Ordinary HTTP Proxy Request

1. Require an absolute-form HTTP URL and validate its scheme, authority, and port.
2. Preserve an immutable OriginalRequest view, then create an outbound request with `RequestURI` cleared and proxy-only headers removed.
3. Build an Exchange with an effective request and one request-body forwarding owner.
4. Run the middleware chain.
5. The terminal handler calls the shared upstream RoundTripper unless a hook returned a response or a typed rejection.
6. Let response middleware inspect headers and wrap the response body before it is written.
7. Copy headers and stream the response body to the client.
8. Record completion only after the client write completes. Report upstream read and client write errors separately.

### CONNECT Passthrough

1. Parse and validate the CONNECT authority as host:port.
2. Evaluate the current MITM policy snapshot for this new connection.
3. If policy denies interception, dial the target and return Connection Established only after the upstream connection succeeds.
4. Relay both directions with half-close support: EOF in one direction half-closes the matching writer, then the proxy continues the other direction until it also closes or the tunnel deadline expires. Track the tunnel and cancel it during shutdown.
5. Treat encrypted bytes as opaque. Do not create synthetic decrypted request records.

### CONNECT with MITM

1. Select the intercepted path using the same immutable policy snapshot.
2. Acquire or generate the leaf certificate before acknowledging CONNECT. If that fails, return a proxy error while the connection is still HTTP.
3. Hijack the client connection, send `200 Connection Established`, and perform the client-facing TLS handshake with the certificate for the CONNECT authority.
4. Advertise only HTTP/1.1 until HTTP/2 MITM is implemented. Do not advertise h2 and then parse HTTP/1.1 bytes.
5. If TLS SNI is present, require it and each decrypted request authority to match the parent CONNECT authority after normalization. Reject mismatches unless a separate explicit policy allows them; an absent SNI is valid for IP-literal CONNECT targets.
6. Serve the established TLS connection through `http.Server` and the MITM ingress adapter, which calls the same Exchange handler as plain HTTP.
7. Construct the original HTTPS URL from the decrypted request authority and request target, then adapt it into a normal Exchange.
8. Route the effective request URL through the shared upstream transport. The final URL determines dial host and TLS ServerName together.
9. Keep the client-facing TLS connection alive across requests when protocol semantics allow.
10. On WebSocket upgrade, hand off to the duplex relay after the handshake. Frame inspection is a later feature.
11. Record each Exchange separately and associate it with the parent CONNECT tunnel.

## 7. Onion Middleware Semantics

The middleware onion is the synchronous control-flow model. Middleware receives a downstream handler and returns a handler around it. The handler returns a response so the onion can process response headers and wrap the response body before the single downstream writer starts copying it.

The conceptual types are:

    import "net/http"

    type ExchangeHandler func(*Exchange) (*http.Response, error)
    type Middleware interface {
        ID() string
        Priority() int
        Wrap(next ExchangeHandler) ExchangeHandler
    }

The pipeline is composed once per published snapshot, from the innermost terminal outward:

    handler := upstreamTerminal
    for i := len(snapshot) - 1; i >= 0; i-- {
        handler = snapshot[i].Wrap(handler)
    }

For a request, the outer middleware runs first and may inspect or patch the effective request. The next middleware sees those changes. The terminal performs the upstream RoundTrip. When it returns a response, response behavior runs in reverse order. Only after the complete chain returns does the ingress adapter write the response to the client.

Hooks are the stable callback surface built on this composition. Hooks return declarative decisions and patches; the adapter validates and applies each patch atomically. A request hook can continue, return a synthetic response, or reject the exchange. A response hook can inspect a response view and return a status/header/body patch. A hook adapter stores any match result and state in the Exchange, never in a shared middleware instance. The conceptual contract is:

    type Hook interface {
        BeforeRequest(*HookContext) (Decision, error)
        AfterResponse(*HookContext, ResponseView) (ResponsePatch, error)
    }

`BeforeRequest` and `AfterResponse` are synchronous. `HookContext` exposes read-only original/effective views, the selected snapshot generation, and bounded body access. `Decision` contains a decision kind plus an ordered request patch or synthetic response. `ResponsePatch` contains optional status/header operations and an optional bounded body replacement. The host validates the entire patch before applying it to a copy. Rule scripts receive only a serialized immutable view and return declarative patches; they do not receive Go request/response objects. Trusted built-in Go middleware may use a body wrapper internally, but that is not part of the dynamic-rule contract.

| Stage | Order | Allowed work |
|---|---|---|
| Before request | Outer to inner | Match, return a validated request patch, continue, synthesize a response, or reject |
| After response headers | Inner to outer | Inspect status/headers, return a validated response patch, or request a bounded body replacement |
| Completion | After downstream body copy | Finalize capture and trace, then publish best-effort events; this stage cannot change the response |
| Failure | Bubbles back through middleware | Record the failing stage; the rule adapter does not silently convert transport errors into success |

### Required Contract

- A middleware calls next zero or one times. Zero calls means it supplies a synthetic response or returns a typed rejection. Retries belong to the transport/retry policy, not arbitrary middleware.
- If a middleware supplies a response, that middleware's response hook and all outer response hooks still run during unwind. Inner middleware that was never entered does not run.
- A middleware records its own match state and trace in per-Exchange state. No request-specific mutable state is stored in the shared middleware instance.
- Request actions remain ordered within each rule. Rules are applied sequentially through the onion, so a later patch observes and may override earlier effective values.
- The original request snapshot never changes. The effective request is changed as the onion runs.
- Matching uses the effective request at that middleware’s entry. A rewrite by an outer middleware is visible to inner middleware. The trace retains original and effective values.
- Priority sorts request traversal from low to high, then by stable rule ID. Response traversal naturally unwinds in reverse.
- The chain catches panics at its outer boundary, records a sanitized middleware failure, and prevents a process crash.
- Middleware and rules do not receive the downstream ResponseWriter or raw socket. Duplex upgrades use a separate, explicit transport contract.
- `Reject` is converted to a defined HTTP error response by the ingress boundary. The chain never silently closes an HTTP request without a response.

### Hooks and Events Are Different

Hooks are synchronous callbacks inside middleware and may affect forwarding. `BeforeRequest` runs on the inward path; `AfterResponse` runs on the outward path. Hook adapters return a bounded body replacement before downstream copying begins; trusted built-in Go middleware may wrap a body internally. Errors while a body is later copied are completion errors, not errors that the middleware onion can catch synchronously.

Events are asynchronous observations for clients and history. They cannot block, continue, rewrite, or cancel forwarding. A slow event subscriber may lose live events without changing the Exchange result.

This distinction avoids making EventHub a hidden second request pipeline.

### Hook Decision

Each request hook returns one of:

- `Continue`: invoke the next handler once.
- `Respond`: supply a synthetic HTTP response and unwind without contacting upstream.
- `Reject`: stop before upstream and return a defined proxy error response.

Replace the current silent `abort` behavior with an explicit rejection outcome. It is deterministic for clients and keeps the Exchange completion record meaningful. For response processing, hooks return no changes or a validated status/header patch and bounded body replacement. A body replacement is prepared completely within the transform limit before it is committed.

## 8. Dynamic Rules on the Middleware Chain

Each validated dynamic rule is compiled into one middleware wrapper. The initial wrapper adapts the existing request-stage JavaScript contract; the middleware interface also provides the response hook seam for staged response-rule support. The wrapper:

1. Matches the Exchange’s effective request at entry.
2. If unmatched, calls next once.
3. If matched, evaluates the rule in an isolated Goja runtime with a timeout and bounded input.
4. Validates the complete rule result before applying any action from that rule.
5. Applies the ordered request patch, then calls next, or returns one synthetic/rejection outcome and skips next. A rule cannot combine conflicting terminal outcomes such as both block and mock.
6. Applies any validated response actions while unwinding when the rule matched. Adding those action schemas is a separate implementation step; the current chiling schema only defines request-stage actions.
7. Records rule ID, version, match, actions, duration, error, and whether a body action was skipped.

Each rule sees the effective request produced by outer middleware. This intentionally replaces the earlier “all rules see one immutable request snapshot” decision. Original request data remains available for inspection and trace output.

### Rule Snapshot Publication

- Rule create/update/enable/disable builds a candidate from a copy of the canonical definitions, validates and compiles it into a new immutable RuleSnapshot, and only then commits the definitions and active snapshot together.
- The snapshot contains deterministically ordered middleware and the corresponding composed handler.
- Publish the new snapshot atomically. A request loads one snapshot and uses it for its entire lifetime.
- Existing Exchanges keep their old snapshot. New requests use the new snapshot.
- A failed update leaves the last valid snapshot active and returns a validation error.
- Do not return mutable pointers into the store to request goroutines. A failed update must leave both the control API's stored definition and the active snapshot unchanged.

The current JavaScript sandbox remains isolated from filesystem, network, process, and environment access. The middleware interface is internal and does not imply a plugin runtime or new script capabilities.

## 9. Request and Response Body Lifecycle

The proxy must not consume a body just to inspect it. Every body has one forwarding owner and an optional non-destructive observation view.

### Capture

- When body capture and body-aware rules are both disabled, do not read a body before RoundTrip; let the transport stream it from the client.
- A body-aware request rule may opt into a bounded preview. Read at most `previewLimit + 1` bytes, record whether the preview is truncated, then replay every consumed byte before the unread remainder. This read-ahead cost applies only when such a rule is enabled.
- Capture enabled bodies with a tee as they are forwarded. Retain at most `captureLimit` bytes and observe one extra byte to mark truncation; do not buffer the full stream just to capture it.
- The upstream request and downstream response receive the full body when no transform is applied, even when the preview or capture is truncated.
- Store captured bytes in BodyCache only after the read limit is enforced. A cache limit alone is not a read limit.
- Preserve read errors. Never mark a partially read body as a successful complete exchange.
- Preserve repeated header values internally and in captured metadata. In particular, do not join `Set-Cookie` fields into one comma-separated value.

### Rule Access and Transformation

- Rules receive a body preview plus explicit `truncated` and observed-length metadata. Text is supplied only when valid under the declared text encoding; otherwise bytes are represented as base64. They do not receive an unbounded string created by `io.ReadAll`; body predicates operate on the documented preview only.
- Request or response body replacement must declare bounded access and must either read the complete body within a transform limit or leave the original body intact.
- An oversized transform is atomic: never forward a partial replacement. Replay any consumed original prefix before the unread remainder, record that the rule was skipped or failed, and follow its failure mode.
- If an enabled response rule may rewrite the body, request identity encoding upstream where possible. Disable implicit transport decompression so capture and rewrite code know which representation they own. If the origin still returns an unsupported or oversized encoding, preserve the original stream and skip the rewrite. When a supported encoding is decoded and a replacement is committed, remove stale encoding metadata and recalculate Content-Length.
- Handle no-body statuses, HEAD, trailers, Content-Length, Transfer-Encoding, and Expect: 100-continue explicitly.
- Preserve binary bytes; do not round-trip arbitrary payloads through lossy text conversion.

Capture limit, rule-preview limit, and transform limit are separate settings because they protect different costs. Add a per-Exchange memory budget and a global active-body budget; a per-body limit alone does not bound aggregate memory under concurrency.

The initial per-body and shared-cache defaults can retain the existing 64 KiB and 10 MiB values. The important change is that these values become enforced read/retention limits, rather than truncating only after an unbounded read.

## 10. Upstream HTTP Transport

- Use one shared http.Transport and a thin client/round-tripper wrapper.
- Pass the Exchange context to RoundTrip so client cancellation stops DNS/dial/read/write work.
- Set the shared transport's `Proxy` field explicitly to `nil` for direct origin routing. An upstream proxy, if later supported, is explicit configuration rather than an accidental `HTTP_PROXY` environment fallback.
- Keep TLS certificate verification enabled. Derive ServerName from the effective target URL after rule rewrites.
- A `rewriteURL` patch changes the URL authority, dial target, TLS ServerName, and outbound Host authority as one validated operation. An intentional virtual-host override must use a separate explicit action.
- For absolute-form HTTP ingress, clear the server-side `RequestURI` before RoundTrip. Remove `Proxy-Authorization`, `Proxy-Connection`, fixed hop-by-hop headers, and headers nominated by `Connection`; do not forward proxy credentials to the origin.
- Use a Dialer timeout, TLS handshake timeout, response-header timeout, idle connection timeout, and request cancellation. Avoid one fixed total timeout that makes long streaming responses impossible.
- Reuse upstream connections where protocol and response semantics permit.
- Normalize header names case-insensitively and preserve multi-value headers. Handle hop-by-hop headers using both the fixed list and `Connection`-nominated tokens; handle `Proxy-Connection`, `Upgrade`, and `Trailer` deliberately.
- Keep `Request.Host` separate from Header fields. Preserve query strings, request bodies, trailers, and content length during normalization.
- Configure the downstream server with header and idle timeouts rather than a total read timeout that breaks long request bodies. Flush streaming responses according to their protocol and preserve client cancellation.
- Do not report a response complete before its body has been written to the client.

## 11. CONNECT Policy and Lifecycle

- One immutable NetworkPolicy owns MITM enabled state, allow patterns, deny patterns, TLS protocol policy, and a generation number. Listener bind and timeout settings belong to startup configuration because changing them may require a listener restart.
- Empty MITM allowlist means deny interception. A wildcard requires explicit configuration. Deny rules take precedence.
- Normalize CONNECT authorities before matching: lowercase DNS names, normalize IDNA and trailing dots, parse bracketed IPv6, and account for the effective port. Host patterns use exact match or a dot-boundary wildcard, not an arbitrary string suffix. Rule hosts never expand MITM permission implicitly.
- The policy is read once when a new CONNECT arrives. Runtime policy changes apply to new tunnels; an established tunnel keeps its original route.
- Once interception is selected, a TLS handshake or HTTP authority validation failure is recorded and closed. Never silently downgrade a failed MITM connection to an opaque tunnel.
- A control API update publishes one new policy snapshot. There is no separate Proxy.MITMEnabled flag.
- The proxy tracks active CONNECT passthroughs, intercepted TLS connections, and Exchanges. Shutdown stops accepting new connections, cancels in-flight Exchanges, closes tracked tunnels after a drain deadline, and waits for proxy-owned goroutines.
- Certificate generation remains in internal/mitm. The CA private key remains owner-readable only. Upstream TLS verification is independent of client-facing generated certificates.

## 12. HTTP and WebSocket Protocol Scope

| Path | Initial support |
|---|---|
| HTTP proxy request | HTTP/1.1 request/response with streaming |
| CONNECT passthrough | Opaque TCP relay; TLS inside may use HTTP/1.1, HTTP/2, or WebSocket |
| CONNECT MITM | HTTP/1.1 request/response through Exchange middleware |
| WebSocket over plain HTTP or MITM | Transparent duplex relay after the 101 handshake; no frame rules yet |
| HTTP/2 MITM | Deferred; do not advertise h2 |

If an HTTP path cannot relay a protocol upgrade, it must fail explicitly before pretending the upgraded connection is supported. Silently treating frames as another HTTP request is invalid. A WebSocket exchange completes when both relay directions finish, not when the 101 response is written.

## 13. Capture, Events, and Trace

- Create an Exchange record once the HTTP request is parsed. For decrypted HTTP, also store the parent tunnel ID.
- Record original URL/headers separately from the effective request after middleware, including the rule snapshot generation and the CONNECT policy generation that admitted an intercepted tunnel.
- Preserve repeated header values and report captured body byte count, truncation, and whether the body completed successfully.
- Emit request-started after parsing; rule events at the rule middleware boundary; response-headers after response headers exist; response-completed only after the downstream copy succeeds.
- Distinguish failures by stage: parse, client body read, middleware, dial, TLS, upstream request write, upstream response read, response middleware, downstream response write, and tunnel relay.
- Event payloads are best-effort observations. The request record and bounded body references remain the query source for recent traffic.
- Do not put request bodies, Authorization, Cookie, or other secrets into logs or event payloads. Provide explicit redaction for captured headers.

## 14. Security and Resource Boundaries

- The proxy listener binds to loopback by default; a non-loopback proxy listener requires authentication. The control API uses an owner-only Unix socket (`0600`); optional TCP control access binds to loopback and requires authentication. The existing `0666` Unix socket mode must be removed before body retrieval or policy mutation is exposed to multiple local users.
- MITM is opt-in and allowlist-based. A broad wildcard is an explicit operator decision.
- Rule output validation is all-or-nothing. Validate URL schemes/hosts, reject URL userinfo, validate header names/values, status codes, and action sizes before applying changes.
- Dynamic scripts keep the current execution timeout and output cap. Add an input cap for the rule source and a per-request total rule budget.
- Treat rule scripts as operator-controlled but potentially buggy. Goja timeouts and input/output caps do not impose a hard VM heap limit; if arbitrary third-party scripts are ever accepted, execute them in a separately resource-limited worker before treating them as untrusted extensions.
- Capture and transform limits bound memory independently of body-cache capacity and active request count. Add a connection/concurrency limit or backpressure policy.
- Body capture must have a documented retention policy and a way to disable body capture while preserving metadata.
- Error logs contain stage and safe error class, not credentials or full request content.
- The rule runtime receives a bounded immutable request/response view and returns declarative actions. Trusted Go middleware must still be reviewed because it can access the process.
- Upstream TLS verification stays enabled. Client-facing CA material and upstream trust are separate; do not expose the CA private key through the control API or rule runtime.

## 15. Failure Semantics

- Invalid middleware or rule configuration is rejected before publication; the last valid snapshot remains active.
- A rule evaluation or validation error discards all actions from that rule, records the error, and follows that rule's declared failure mode. No partial set of valid actions is applied from an invalid result.
- Malformed client requests return 400; upstream timeouts return 504; upstream dial, TLS, and protocol failures return 502. Do not include raw upstream error strings in client responses.
- Native transport errors return a proxy error response and a failed Exchange record.
- Client cancellation closes the upstream request and records cancellation distinctly from an origin failure.
- Downstream write errors are not converted into successful completion.
- If a response middleware fails before body copy, discard its atomic patch and preserve the prior response when possible. If a streaming body wrapper fails during copy, stop the downstream write and record a response-body failure; do not claim success.
- A rule's failure mode is explicit: `fail-open` skips that rule and continues with the current Exchange; `fail-closed` returns a configured error response. Fail-open is the proposed default to preserve application traffic; enforcement rules opt into fail-closed.

## 16. Acceptance Criteria

The network implementation is ready when these behaviors hold:

1. A plain HTTP request body reaches the origin byte-for-byte when no rule changes it, including bodies larger than the capture limit.
2. A response can stream its first chunk to the client before the origin finishes the body when no middleware needs full-body transformation.
3. A body capture limit truncates only captured bytes, not forwarded bytes.
4. A bounded request preview is replayed byte-for-byte to upstream; with no body-aware hook, the proxy does not read ahead even when capture is enabled.
5. A client disconnect cancels the corresponding upstream request.
6. Multi-value headers survive round-trip, and proxy credentials never reach the origin.
7. MITM status and CONNECT routing read the same policy generation; enable/disable affects new tunnels as documented.
8. A rewritten MITM host changes the actual dial target, TLS ServerName, and outbound Host authority together.
9. A rule that returns an invalid action applies none of its actions.
10. Equal-priority middleware order is stable across reloads and process runs.
11. A synthetic response short-circuits upstream and still unwinds the responding and outer response middleware.
12. MITM passthrough hosts remain opaque, and intercepted WebSocket upgrades relay both directions without corrupting frames.
13. Shutdown closes active tunnels and does not leave proxy-owned goroutines blocked.
14. Unauthorized local or remote clients cannot read captured bodies or mutate MITM/rule policy.

## 17. Implementation Sequence

1. Introduce Exchange, immutable NetworkPolicy, middleware composition, and a shared upstream RoundTripper. Route ordinary HTTP through it and fix request-body forwarding.
2. Move response writing and capture to bounded streaming. Add body preview/truncation metadata and correct event completion timing.
3. Replace direct MITM fields with atomic policy publication; make enable/disable and allowlist updates update one source of truth.
4. Serve decrypted HTTP/1.1 through the shared `http.Server` handler. Remove the duplicate request/action implementation from connect.go.
5. Compile dynamic rules into hook adapters; stabilize snapshots, action ordering, trace data, and error policy. Add response action schemas only after response streaming and body limits are in place.
6. Add active tunnel lifecycle and transparent WebSocket upgrade relay. Keep frame-level rules deferred.

Each step should preserve a working proxy. The first implementation milestone should include focused behavior verification for body forwarding, streaming, MITM policy updates, URL rewriting, and shutdown before broadening rule features.

## 18. Open Design Choices

| Choice | Recommended direction | Why it remains open |
|---|---|---|
| Rule match input | Effective request at each rule entry, with original request kept separately | This changes the earlier no-chaining rule semantics and affects existing rules |
| Rule error mode | Fail-open by default; fail-closed must be explicit and include a response | Confirm whether enforcement rules should fail closed automatically when evaluation times out |
| Oversized body transform | Discard the transformation atomically and record “skipped”; follow the rule's failure mode | Confirm whether a fail-closed rule should reject when the body exceeds its transform limit |
| MITM empty allowlist | Deny by default | Existing behavior treats empty as all, so this is a visible policy change |
| CONNECT and decrypted Host mismatch | Reject by default unless an explicit route policy permits it | CONNECT authority, TLS SNI, and HTTP Host can differ; silently choosing one can route outside the operator's expected scope |
| WebSocket scope | Transparent upgrade relay now; frame-level rule hooks later | Frame capture and mutation add a distinct bidirectional lifecycle |
| HTTP/2 MITM | Defer; opaque CONNECT continues to work | HTTP/2 interception requires a deliberate protocol and compatibility plan |
