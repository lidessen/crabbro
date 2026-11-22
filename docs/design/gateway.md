# Crabbro Multi-Protocol LLM and Decision Gateway

Status: Implemented  
Date: 2026-11-19  
Scope: Prerequisite shared-core refactoring and an independent local LLM & Decision gateway subsystem inside `crabbrod` that aggregates API-key coding plans across `openai-chat`, `openai-responses`, `anthropic`, `gemini`, `typesafe-jev`, and `openai-decisions`.

Cross-references: [NETWORK_PIPELINE_DESIGN.md](../../NETWORK_PIPELINE_DESIGN.md), [rules.md](./rules.md), [plugins.md](./plugins.md), [ui-api.md](./ui-api.md), [gateway-plan.v1.json](../../schemas/gateway-plan.v1.json), [gateway-route.v1.json](../../schemas/gateway-route.v1.json).

---

## 1. Decision summary

1. **Peer data planes, not forward-proxy rules.** The LLM gateway is a dedicated origin-form reverse proxy subsystem (`internal/gateway`, default listener `127.0.0.1:8081`) inside `crabbrod`. It does not run inside `internal/proxy`'s forward-proxy rule onion (64 KiB body previews, 256 KiB buffered response transforms, 60 s response-header timeouts).
2. **Extract four shared core capabilities first.** Before adding gateway data-plane code, extract `internal/secret`, an `ExchangeSink` observation interface, a non-pruned quota ledger (`usage.db`), and unify `internal/decision` over the gateway's `decision` route.
3. **Four orthogonal primitives.** Separate **Protocol** (wire codec), **Plan** (one API key + shared quota windows + 1..N protocol endpoints + declared model specs), **Route** (virtual model matching + scheduling policy + ordered targets), and **Profile** (`/p/{profile}/...` URL scope).
4. **Never mutate external agent config files.** Clients (`Claude Code`, `Codex`, `OpenCode`, `Gemini CLI`, or custom tools) point their `*_BASE_URL` env var or setting at the gateway or a `/p/{profile}/` prefix. `crabbrod` never edits `~/.claude/settings.json` or `~/.codex/config.toml`.
5. **Two semantic families across six wire codecs.** `openai-chat`, `openai-responses`, `anthropic`, and `gemini` belong to the **Generative** family; `typesafe-jev` and `openai-decisions` (`POST /v1/decisions`) belong to the **Decision** family and bridge through DecisionIR. `Generative -> Decision` is rejected at config validation time. `Decision -> Generative` is allowed as explicit, flagged emulation (`X-Crabbro-Decision-Mode: emulated`, `calibrated: false`).
6. **Same-protocol Fast Path; IR Bridge only when protocols differ.** When `clientProtocol == chosenEndpoint.Protocol`, requests pass through with only a surgical top-level `"model"` JSON patch, static header/extraBody injection, and a read-only streaming `UsageTap`. Cross-protocol calls translate through `GenIR` or `DecisionIR -> GenIR` without an `N×N` converter matrix.
7. **One unified multi-objective router (`Quality × Cache × Quota`).** Instead of magpie-style two-step `intent classifier -> soonest-reset account` routing that breaks prompt caches, every generative request runs a 4-stage pipeline: deterministic signal extraction (`0 ms`) -> optional cold-boundary semantic tiering -> hard constraint filtering -> unified utility scoring with full explainable trace.
8. **True pre-commit failover with first-SSE-event peek.** Requests are buffered up to `DefaultMaxRequestBody` (32 MiB) so attempts can retry cleanly. For `text/event-stream` responses, the executor peeks the first meaningful SSE event *before* committing HTTP `200` downstream, catching providers that return HTTP `200` with an immediate rate-limit or quota error payload.
9. **Dedicated SQLite ledger (`usage.db`).** Rolling window accounting lives in `~/.crabbro/gateway/usage.db`, isolated from `history.db` (which prunes after 7 days / 10,000 rows and can be turned off with `--history off`).
10. **Unify `internal/decision`.** `crabbro find`, `crabbro rules draft`, plugin `decision.ask`, and the router's optional cold-boundary semantic tiering all invoke the same in-process gateway `decision` route (`hopDepth=1` recursion guard), automatically gaining Jev + Gemini + OpenAI + Anthropic failover and fixing the stale JavaScript prompt in `internal/decision/openai.go:114`.
11. **Per-file YAML + `draft/validate/commit` + immutable `Snapshot`.** Plans and routes live in `~/.crabbro/gateway/plans/*.yaml` and `routes/*.yaml`, validated against Draft 2020-12 JSON Schemas and published via `atomic.Pointer[Snapshot]`.
12. **Zero-leak secrets.** API keys live only in `~/.crabbro/secrets/` (mode `0600`) and are scrubbed from headers, bodies, SSE events, history, and Gemini `?key=` query strings before capture.

---

## 2. Goals and non-goals

### Goals

- Aggregate multiple API-key coding plans (multi-endpoint plans like GLM/Kimi/MiniMax as well as direct OpenAI, Anthropic, Gemini, and TypeSafe Jev accounts) behind one local loopback daemon.
- Preserve vendor-native features (`cache_control`, extended `thinking` signatures, reasoning items, provider beta headers) losslessly on same-protocol routes.
- Prevent prompt-cache thrashing across tool loops and warm multi-turn sessions while still draining soon-to-expire plan quotas and honoring model quality requirements.
- Unify all LLM observability (forward-proxy traffic and gateway attempts, token/cache accounting, routing score breakdowns) in the same `requests`, `listen`, and MyGo desktop UI surfaces.
- Make routing deterministic, offline-testable (`crabbro gateway preview`), and explainable in JSON traces.

### Non-goals

- OAuth login flows, cookie/browser session scraping, or client-fingerprint spoofing for consumer web subscriptions (Claude Pro/Max, ChatGPT Plus/Codex OAuth, Copilot) inside the core daemon.
- Automated rewriting of third-party CLI configuration files or macOS keychain entries.
- Non-text media modalities (`/v1/audio/*`, `/v1/images/*`, `/v1/embeddings`, Realtime WebSocket API) in v1.
- Evaluating YAML forward-proxy rules (`crabbro.rewrite-response-body`, Goja `run`) over gateway SSE streams.
- Billing-accurate USD accounting; local window tracking optimizes quota utilization and failover using declared budgets, upstream rate-limit headers, and observed tokens.

---

## 3. Prerequisite core refactoring in Crabbro

Today `crabbrod` couples capture stores (`RingBuffer`, `BodyCache`, `EventHub`, `Sessions`, `History`) to `internal/proxy`, stores secrets inside `internal/plugin`, and hardcodes direct HTTP calls for `jev` and `openai` in `internal/decision`. Refactoring those four boundaries makes `ForwardProxy` and `Gateway` clean peers over one shared platform:

```text
                ┌──────────────────────────────────────────┐
                │   CLI (crabbro)  /  Desktop UI (MyGo)    │
                └────────────────────┬─────────────────────┘
                                     │ /v1 (UDS 0600 / loopback TCP + Bearer)
┌────────────────────────────────────▼────────────────────────────────────┐
│                                crabbrod                                 │
│                                                                         │
│  ┌───────────────────────────────┐  ┌────────────────────────────────┐  │
│  │ Forward Proxy (:8080)         │  │ LLM & Decision Gateway (:8081) │  │
│  │ • HTTP / CONNECT / MITM / WS  │  │ • Origin-form + /p/{profile}/  │  │
│  │ • YAML pipeline + plugins     │  │ • 6 Codecs + Fast/IR paths     │  │
│  │ • 64K preview / 256K rewrite  │  │ • Unified Router + Peek Retry  │  │
│  └──────────────┬────────────────┘  └───────────────┬────────────────┘  │
│                 │                                   │                   │
│                 └──────────────────┬────────────────┘                   │
│                                    ▼                                    │
│  ┌───────────────────────────────────────────────────────────────────┐  │
│  │                        Shared Core Layer                          │  │
│  │ • internal/secret   : ~/.crabbro/secrets/ (0600) + Scrub helpers  │  │
│  │ • ExchangeSink      : RingBuffer + BodyCache + EventHub + History │  │
│  │ • Quota Ledger      : ~/.crabbro/gateway/usage.db (non-pruned)    │  │
│  │ • internal/decision : unified client backed by gateway `decision` │  │
│  │ • Config Lifecycle  : draft / validate / commit / Snapshot        │  │
│  └───────────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────┘
```

### 3.1 `internal/secret` (extracted from `internal/plugin`)

Currently `internal/plugin/store.go` (line 228) and `internal/plugin/host.go` (lines 183–200) read credential files directly from `~/.crabbro/plugins/secrets/<ref>`, while `scrubAction` and `scrubMap` live inside `internal/plugin/host.go`.

Extraction plan:
- Move secret storage to `internal/secret.Store` rooted exclusively at `~/.crabbro/secrets/` (files mode `0600`, symlinks and path traversal rejected, valid ref pattern `^[a-z0-9][a-z0-9._-]{0,63}$`).
- Update `internal/plugin` to depend directly on `*secret.Store`, dropping its old `~/.crabbro/plugins/secrets/` directory and local scrubbing helpers.
- Provide `secret.Scrubber` helpers used by both `internal/plugin` and `internal/gateway`:
  - `ScrubHeaders(http.Header)` — strips `Authorization`, `x-api-key`, `x-goog-api-key`, `Proxy-Authorization`, and any configured `endpoint.authHeader`.
  - `ScrubURL(rawURL string) string` — strips sensitive query parameters (`key`, `api_key`, `access_token`), specifically preventing Gemini `?key=AIza...` parameters from entering `URL`, `OriginalURL`, `EffectiveURL`, logs, or `history.db`.
  - `ScrubText(s string, values ...string) string` — replaces any resolved credential value (non-empty, length >= 6) with `[REDACTED]`.

### 3.2 `ExchangeSink` and `pkg/api` extensions

Currently `internal/proxy/exchange.go` directly writes into `p.RingBuffer`, `p.BodyCache`, `p.EventHub`, `p.Sessions`, and `p.History`.

Extraction plan:
- Define a small capture interface (`ExchangeSink` in `internal/shared` or `internal/capture`) encapsulating `Start(req *shared.Request)`, `StoreBody(b []byte) shared.BodyRef`, `Emit(typ shared.EventType, id shared.RequestID, payload any)`, `NoteActivity(...)`, and `Complete(req *shared.Request, reqBody, respBody []byte)`.
- Replace the direct capture-store fields and write paths on `internal/proxy.Proxy` with `ExchangeSink`, and have both `internal/proxy` and `internal/gateway` emit records through the same `ExchangeSink` (no dual write paths). Every gateway exchange gets an `req_<n>` ID, appears in `crabbro requests list/show`, streams over `crabbro listen`, and persists to `history.db` when history is enabled.
- Extend `pkg/api/request.go` with explicit traffic fields (keeping `pkg/api` and `pkg/client` strictly free of `internal/` imports per [ui-api.md](./ui-api.md)):
  - `Kind string` (always explicitly set to `"proxy"` or `"gateway"`).
  - `Gateway *GatewayTrace` (`omitempty`, set when `Kind == "gateway"`) containing:
    - `profile`, `routeId`, `family` (`"generative" | "decision"`), `ingressProtocol`
    - `requestedModel`, `chosenPlan`, `chosenModel`, `upstreamProtocol`, `translated` (`bool`), `decisionMode` (`"native" | "emulated"`, if `decision`)
    - `turnPhase`, `sessionKey`, `cacheGravity`, `routeDecision` (score breakdown)
    - `attempts []GatewayAttempt` (`plan`, `model`, `protocol`, `status`, `errorKind`, `latencyMs`, `ttftMs`, `cooldownApplied`)
    - `usage *TokenUsage` (`inputTokens`, `outputTokens`, `cacheReadTokens`, `cacheWriteTokens`, `reasoningTokens`)
- Extend `pkg/api/event.go` with `gateway_config_changed` and `gateway_plan_health_changed` (observational SSE events only; never steer execution).

### 3.3 Separate quota ledger (`~/.crabbro/gateway/usage.db`)

`internal/history/store.go` enforces a default retention of 7 days and 10,000 rows (`Prune()` runs on every `Save()`), and `--history off` disables `history.db` entirely. Gateway quota tracking for 7-day or 30-day rolling windows cannot rely on a pruned capture table or disappear when body capture is turned off.

Add a lightweight dedicated SQLite database `~/.crabbro/gateway/usage.db` storing only accounting metadata (no request/response bodies, no secrets):

```sql
CREATE TABLE IF NOT EXISTS gateway_usage (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                 INTEGER NOT NULL, -- Unix epoch milliseconds
  plan_id            TEXT NOT NULL,
  model              TEXT NOT NULL,
  protocol           TEXT NOT NULL,
  route_id           TEXT NOT NULL,
  session_key        TEXT NOT NULL,
  status             INTEGER NOT NULL,
  error_kind         TEXT NOT NULL DEFAULT '',
  input_tokens       INTEGER NOT NULL DEFAULT 0,
  output_tokens      INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
  total_tokens       INTEGER NOT NULL DEFAULT 0,
  ttft_ms            INTEGER NOT NULL DEFAULT 0,
  duration_ms        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_gw_usage_plan_ts ON gateway_usage(plan_id, ts);
CREATE INDEX IF NOT EXISTS idx_gw_usage_ts ON gateway_usage(ts);
```

Retention defaults to 90 days (configurable via `--gateway-usage-retention`), keeping the file tiny while accurately feeding rolling quota windows and usage dashboards.

### 3.4 `internal/decision` unification

Currently `internal/decision/decision.go` only accepts `Provider == "jev" || Provider == "openai"`, reads environment variables directly (`FromEnv()`), and duplicates HTTP logic across `find*`, `draft*`, and `ask*`. In addition, `internal/decision/openai.go:114` still prompts for the removed JavaScript rule format (`"Write a crabbro JavaScript rule..."`).

Unification plan:
- Refactor `decision.Client` (`Find`, `Draft`, `Ask`) to invoke the gateway's `"decision"` route in-process via a `DecisionInvoker` interface (with `hopDepth = 1` on the internal context so `decision` never recurses into semantic routing).
- Delete the direct provider HTTP callers in `jev.go` and `openai.go`, `FromEnv()`, `CRABBRO_DECISION_*` env parsing, and `crabbrod --decision-*` CLI flags. When no enabled `"decision"` route is configured in `~/.crabbro/gateway/routes/`, `decision.Client` returns `ErrNotConfigured` directly (no hidden in-memory fallback route synthesis).
- Fix `Draft` prompts to emit valid YAML v1 rule pipelines (`schemas/rule.v1.json`) rather than old JavaScript rules.
- Result: `crabbro find`, `crabbro rules draft`, plugin `decision.ask`, and cold-boundary semantic routing share one code path and automatically support native Jev + Gemini + OpenAI + Anthropic with failover, cooldowns, and unified request traces.

---

## 4. Package ownership and layout

| Package | Owns | Must NOT own |
|---|---|---|
| `internal/secret` | `~/.crabbro/secrets/` `0600` file store, URL/header/text scrubbing | HTTP listeners, YAML config parsing |
| `internal/gateway` | Listener (`:8081`), profile/path routing (`/p/{profile}/...`), `ExchangeSink` recording, lifecycle (`Start`/`Stop`) | Forward-proxy CONNECT/MITM, Goja/Node plugin evaluation |
| `internal/gateway/config` | Plan & route YAML loading, Draft 2020-12 schema validation, cross-file checks, `draft/validate/commit/discard`, immutable `Snapshot` | Live cooldowns, token counters, HTTP transport |
| `internal/gateway/ir` | Canonical `GenRequest`, `GenResponse`, `StreamEvent`, `TokenUsage`, `DecisionRequest`, `DecisionResponse`, `DecisionIR -> GenIR` adapter | Wire JSON serialization or vendor HTTP headers |
| `internal/gateway/codec` | 6 wire codecs (`openai-chat`, `openai-responses`, `anthropic`, `gemini`, `typesafe-jev`, `openai-decisions`): Fast Path `"model"` patcher + `UsageTap`, and Bridge Path `Decode`/`Encode`/stream state machines | Retry loops, plan selection, SQLite writes |
| `internal/gateway/router` | `SignalExtractor`, cold-boundary `SemanticTiering` hook, hard filters, `Quality × Cache × Quota` utility scorer, `SessionCacheStore` | HTTP round-trips, body buffering |
| `internal/gateway/executor` | Request body buffering (`<= 32 MiB`), attempt loop, first-SSE-event peek, error classifier, cooldown manager, per-plan `http.Transport` pool | Scoring weights, YAML file I/O |
| `internal/gateway/ledger` | `usage.db` SQLite store, rolling window aggregator, upstream rate-limit header calibration | Request body storage |
| `internal/decision` | High-level `Find` / `Draft` / `Ask` prompts, strict result validation, `DecisionInvoker` call into the gateway `decision` route | Direct provider HTTP boilerplate or env config |

---

## 5. Protocols, semantic families, and dual-path data plane

### 5.1 Supported protocols and endpoints

Profiles scope requests without touching external client config files:
- Default profile (`""`): `/v1/...` or `/v1beta/...`
- Named profile: `/p/{profile}/v1/...` or `/p/{profile}/v1beta/...`

| Protocol ID | Ingress & Upstream Endpoints | Semantic Family | Auth Extraction (Ingress -> Stripped Before Trace) |
|---|---|---|---|
| `openai-chat` | `POST /v1/chat/completions` | Generative | `Authorization: Bearer ...` |
| `openai-responses` | `POST /v1/responses` | Generative | `Authorization: Bearer ...` |
| `anthropic` | `POST /v1/messages`<br>`POST /v1/messages/count_tokens` | Generative | `x-api-key` or `Authorization: Bearer ...` |
| `gemini` | `POST /v1beta/models/{m}:generateContent`<br>`POST /v1beta/models/{m}:streamGenerateContent` | Generative | `x-goog-api-key`, `Authorization: Bearer ...`, or `?key=` (stripped immediately) |
| `typesafe-jev` | `POST /v1/systemone` | Decision | `Authorization: Bearer ...` |

Shared metadata endpoint: `GET /v1/models` (and `GET /p/{profile}/v1/models`) returns enabled route IDs and literal model aliases, plus permitted direct `<plan>/<model>` addresses. Entries are sorted and deduplicated, and include `crabbro_kind` and `family`; direct entries use the plan ID as `owned_by`.

### 5.2 Semantic family routing matrix

| Ingress Family | Target Endpoint Family | Allowed? | Behavior & Headers |
|---|---|---|---|
| Generative | Generative (same protocol) | Yes | **Fast Path** passthrough; `X-Crabbro-Route`, `X-Crabbro-Plan`, `X-Crabbro-Attempt` |
| Generative | Generative (different protocol) | Yes (when `allowCrossProtocol: true`) | **Bridge Path** via `GenIR`; sets `X-Crabbro-Translated: 1` |
| Generative | Decision (`typesafe-jev`) | **No** | Rejected at `config validate` and runtime; System One does not generate text/code |
| Decision | Decision (`typesafe-jev`) | Yes | **Fast Path** passthrough; `X-Crabbro-Decision-Mode: native`, `calibrated: true` |
| Decision | Generative | Yes (when `allowEmulation: true`) | **One-way `DecisionIR -> GenIR` emulation**; `X-Crabbro-Decision-Mode: emulated`, `calibrated: false` |

### 5.3 Fast Path (same-protocol passthrough)

When `ingressProtocol == upstreamEndpoint.Protocol` and the client's requested parameters are supported by the endpoint:

1. **Zero full-tree deserialization.** Unmarshal only the top-level JSON object into `map[string]json.RawMessage`, replace `"model"` with the chosen target model (and apply optional `modelSpec.extraBody` defaults for absent keys), and re-marshal. Unknown vendor fields (`cache_control`, `thinking`, `reasoning`, `betas`, `service_tier`, `metadata`) pass through bit-for-bit untouched.
2. **Auth & header hygiene.** Strip client-supplied gateway auth tokens (`Authorization`, `x-api-key`, `x-goog-api-key`, Gemini `?key=`). Inject the plan's credential using the endpoint's `authHeader`/`authPrefix` defaults (`Authorization: Bearer `, `x-api-key: `, or `x-goog-api-key: `), plus non-secret `endpoint.headers`.
3. **Passive `UsageTap`.** On non-streaming responses, inspect a bounded copy of the JSON response to extract token counts and finish reason; on SSE responses, wrap the downstream byte stream with a lightweight line scanner that reads `usage` / `message_start` / `message_delta` / `response.completed` / `usageMetadata` events without buffering or blocking downstream `Flush()`.

### 5.4 Bridge Path (`GenIR` and `DecisionIR`)

Cross-protocol requests translate through canonical in-memory Go IR types (`internal/gateway/ir`), yielding `O(N)` codec implementations (4 Generative codecs + 2 Decision codecs) instead of `N×N` pair adapters.

```go
type TokenUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
}

type GenRequest struct {
	Model          string
	System         []ContentPart
	Messages       []Message
	Tools          []ToolDecl
	ToolChoice     *ToolChoice
	Thinking       *ThinkingConfig
	ResponseSchema map[string]any // structured JSON schema when requested
	MaxTokens      int
	Temperature    *float64
	Stream         bool
	SessionHint    string
}

type ContentPart struct {
	Kind      string // "text" | "image" | "thinking" | "tool_use" | "tool_result"
	Text      string
	Signature string // opaque reasoning/thinking signature (vendor-scoped)
	MediaType string
	Data      []byte // or URL reference
	ToolID    string
	ToolName  string
	InputJSON json.RawMessage
	IsError   bool
}
```

Streaming translation uses a closed union of ~10 `StreamEvent` kinds (`stream_start`, `thinking_delta`, `text_delta`, `tool_call_start`, `tool_call_delta`, `tool_call_end`, `usage_delta`, `stream_end`, `stream_error`). Each upstream codec parses SSE frames into `[]StreamEvent`, and the ingress codec renders them into the client's expected wire stream (synthesizing Anthropic `content_block_start/stop` indices or OpenAI `index` deltas deterministically).

**Thinking-signature safety invariant:** Cryptographic or opaque reasoning signatures (`thinking.signature` in Anthropic, encrypted reasoning items in OpenAI Responses, `thoughtSignature` in Gemini) are **never** forwarded across different providers. When bridging cross-provider, thinking text is either omitted or downgraded according to target rules, and opaque signatures are dropped cleanly so upstream validators do not reject foreign signatures with `400 Bad Request`.

#### `DecisionIR` and one-way `DecisionIR -> GenIR` emulation

```go
type DecisionQuestion struct {
	ID           string
	Kind         string // "noul" | "choice" | "score"
	Instructions string
	Criteria     any    // map[string]string | []string | ordered levels
}

type DecisionRequest struct {
	Model     string
	State     any
	Questions map[string]DecisionQuestion
}
```

When a `decision` route fails over from `typesafe-jev` to a generative endpoint:
- The `DecisionIR -> GenIR` adapter builds a strict JSON Schema matching every question ID and type (`noul` constrained to `[0,1]`, `choice` constrained to enum keys, `score` constrained to `[0,1]` / level enum).
- For `openai-chat`, `openai-responses`, and `gemini`, it maps directly to native structured output (`response_format: json_schema` / `text.format` / `responseSchema`).
- For `anthropic`, it maps to a single forced `tool` with `tool_choice: {type: "tool", name: "submit_decision"}` and extracts the tool input JSON.
- The response explicitly populates `calibrated: false` (and header `X-Crabbro-Decision-Mode: emulated`) rather than pretending an uncalibrated generative answer has System One calibration.

---

## 6. Unified multi-objective router (Cache × Quota × Quality)

Magpie's `smart` routing fails coding-agent workloads because two decisions happen in disconnected layers: an LLM intent classifier picks a tier on the raw latest turn (without knowing cache warmth or quota burn), and then account selection picks the soonest-resetting key (which cuts across warm prompt prefixes and can drain a scarce window). In addition, calling an LLM classifier on every `tool_result` iteration adds latency and breaks reasoning continuity.

Crabbro replaces separate `ordered` / `smart` / `intent` / `sticky` switches with **one 4-stage unified pipeline**.

### Stage 1: Deterministic signal extraction (`0 ms`)

Computed purely from request headers, route config, and JSON payload inspection before touching any external model:

1. **Session key derivation** (first match wins):
   - Explicit header `X-Crabbro-Session`
   - Protocol native field (`metadata.user_id` on Anthropic, `prompt_cache_key` or `previous_response_id` on OpenAI)
   - Stable hash of `(profile, ingressProtocol, systemPromptPrefix[0:1024], firstUserMessagePrefix[0:512])`
2. **Turn phase (`TurnPhase`) classification**:
   - `DecisionCall`: `family == "decision"` (stateless, latency-sensitive).
   - `AuxSubcall`: heuristic match for auxiliary low-stakes subcalls (e.g., Claude Code's small haiku title/summary/compaction prompts: no tools declared and `estimatedInputTokens < 4000`, or requested model matches a fast-tier pattern).
   - `ToolLoop`: trailing message is a `tool_result` / `tool` / `function_call_output` (agent mid-loop continuation; user did not speak).
   - `ColdStart`: first turn (`len(messages) <= 2` or no prior warm entry in `SessionCacheStore`).
   - `NewUserTurn`: subsequent user prompt in an existing session.
3. **Baseline demand & capability flags**:
   - `requestedTier`: inferred from requested model name (`haiku`/`mini`/`flash` -> `fast`, `sonnet`/`coder` -> `strong`, `opus`/`pro`/`max` -> `frontier`; defaults to `strong` for virtual aliases like `coding-main`).
   - `requiredCapabilities`: `{tools, vision, thinking, structuredOutput}` detected from payload fields.
   - `estimatedInputTokens`: fast byte-length estimate (`utf8Bytes / 3.5` plus image token allowances).
4. **Warm cache lookup**:
   - Query in-memory `SessionCacheStore` (bounded LRU, e.g. 4,096 active sessions) for `(sessionKey)`: last `(planID, model)`, `warmPrefixTokens` (from last `cacheReadTokens + inputTokens` or prefix hash match), and `age = now - lastSuccessAt`.

### Stage 2: Optional cold-boundary semantic tiering

`semanticTiering` (using the internal `decision` route) **never picks an account or plan**, and **never runs during `ToolLoop` or `AuxSubcall`**. It runs **only** when all of the following hold:
- `route.policy.semanticTiering.enabled == true`
- `hopDepth == 0` (prevents recursive self-invocation)
- `turnPhase` is `ColdStart` OR (`turnPhase == NewUserTurn` and `cacheGravity < semanticTiering.maxCacheGravity`, meaning cache is cold or naturally expired)

Execution rules:
- Sends a compact sanitized summary (<= 2 KiB: system role summary + latest user turn + tool count + `estimatedInputTokens`, never full repo dumps) to `semanticTiering.decisionRoute` with `decision` question `tier` (`choice: ["fast", "strong", "frontier"]`).
- Bounded by `semanticTiering.timeout` (default `800ms`), strictly **fail-open**: on timeout/error or `confidence < minConfidence`, demand stays at `requestedTier`.
- Even on high confidence, `demand.tier` blends conservatively (never downgrading `frontier` requests when tools + thinking are active unless `confidence >= 0.85`).

### Stage 3: Hard-constraint filtering

Eliminate any target `(plan, endpoint, model)` where:
- Plan or target is disabled, or credential is missing from `internal/secret`.
- `endpointProtocol` violates the semantic family matrix or `allowCrossProtocol == false`.
- Plan is in active failure cooldown (`cooldownUntil > now`) or at `limits.concurrency`.
- A `hard` rolling window in `usage.db` is >= 100% exhausted (unless **all** targets are exhausted, in which case `hard`-blocked targets remain ineligible while `soft`-throttled targets survive as graceful last-resort fallback).
- `modelSpec.contextWindow` (or target `maxContextTokens` / `minContextTokens`) or `capabilities` cannot satisfy the request (`tools`, `vision`, `thinking`, `structuredOutput`).

### Stage 4: Unified multi-objective utility scoring

Every surviving candidate `c = (plan, endpoint, model)` receives a normalized score in `[-10, +10]` (plus small `HealthPenalty`):

```text
Score(c) =
    w.quality  * QualityMatch(c.model, demand)
  + w.cache    * CacheGravity(c, sessionState, turnPhase, estimatedInputTokens)
  + w.quota    * QuotaUrgency(c.plan)
  + w.protocol * NativeProtocolBonus(c.endpoint, ingressProtocol)
  + PriorityBias(c.target)
  - HealthPenalty(c.plan, c.model)
```

#### 4.1 `CacheGravity(c)` — continuous TTL-aware prefix gravity

Let `ttl = firstNonZero(route.policy.cacheTtlOverride, model.cacheTtl, plan.cache.ttl, 5m)` and `age = now - sessionState.lastAt`.
- If `c.(plan, model)` != `sessionState.(lastPlan, lastModel)` or `age >= ttl`: `CacheGravity = 0.0` (cache already expired upstream — **rebalancing is free**).
- If `turnPhase == ToolLoop` and `age < ttl`: `CacheGravity = +10.0` (structural hard pin so tool loops never break reasoning/cache mid-turn unless the candidate fails hard constraints).
- Otherwise (`NewUserTurn` while `age < ttl`):
  `overlapRatio = clamp(sessionState.warmPrefixTokens / max(estimatedInputTokens, 1), 0, 1)`
  `freshness = 1.0 - (age / ttl)` (exponential or linear decay; linear `1 - age/ttl` with a floor of `0.15` while `< ttl` keeps behavior predictable)
  `CacheGravity = overlapRatio * freshness` (in `[0, 1]`).

#### 4.2 `QuotaUrgency(plan)` — burn-rate surplus across multi-window plans

Magpie's "soonest reset first" ignores how much quota remains relative to time left. Instead, for each configured window `w` (`duration`, `maxTokens`/`maxRequests`, `resetSchedule`):

```text
remainingRatio = clamp(1.0 - (usedInWindow / limit), 0, 1)
timeLeftRatio  = clamp(timeUntilReset / w.duration, 0.01, 1)
surplus(w)     = remainingRatio - timeLeftRatio
```

- `surplus > 0` (e.g. 80% quota left, 20% time left -> `+0.60`): plan is **under-burned** and about to waste expiring quota -> positive score boost (weighted higher when `timeLeftRatio` is small).
- `surplus < 0` (e.g. 15% quota left, 70% time left -> `-0.55`): plan is **over-burned** -> negative score so new cold sessions naturally steer away before hitting a `429`.
- Multi-window aggregation (e.g. `5h` + `7d`):
  - If any window is in deficit (`surplus < -0.15`), take `min_w surplus(w)` (bottleneck protection).
  - Otherwise take `max_w (surplus(w) * (1.0 - 0.5*timeLeftRatio(w)))` (burn soonest-expiring surplus first).
- Plans without declared `windows` receive neutral `QuotaUrgency = 0.0` (only RPM/TPM/cooldown apply).

Provider-reported expiry adds a bounded bonus (`<= +0.2`) to `QuotaUrgency`: reset coupons and underutilized windows inside 24h (weekly: 48h) score higher as their expiry approaches and only in proportion to the quota still usable. A plan whose five-hour window is exhausted (`quota_exhausted`, usage `>= 99%`) never receives this bonus, so an expiring coupon cannot outrank a plan with burnable quota.

**Three-tier quota calibration ladder (no vendor balance API required):**
1. **Tier 1 (Exact):** Parse `anthropic-ratelimit-*`, `x-ratelimit-remaining-*`, `x-ratelimit-reset-*` headers when upstream provides them.
2. **Tier 2 (Local ledger):** Aggregate token/request totals from `usage.db` over each window (`rolling` sliding sum or `fixed` calendar boundary).
3. **Tier 3 (Reactive AIMD):** On `429` or quota exhaustion error, mark active cooldown and temporarily cap effective concurrency/rate until recovery.

#### 4.3 `QualityMatch(model, demand)` — asymmetric capability fit

Maps tiers `fast=1`, `strong=2`, `frontier=3` (using `model.codingScore` as fine tie-breaker within `[0, 1]`):
- **Exact tier match:** `+1.0 + 0.2 * codingScore`
- **Over-provisioned** (e.g. `frontier` model serving `strong`/`fast`): `+0.7 + 0.2 * codingScore` (mild preference against wasting top models, easily overridden when a `frontier` plan has high positive `QuotaUrgency` about to expire!)
- **Under-provisioned** (e.g. `fast` model serving `frontier` demand): `-1.5` (strong guard against quality regression; still available as failover if higher tiers are exhausted).

#### 4.4 Preset weight table

| Preset | `w.quality` | `w.cache` | `w.quota` | `w.protocol` | Intended Use |
|---|---|---|---|---|---|
| `balanced` (default) | `1.5` | `2.0` | `1.2` | `0.3` | Protects warm cache, honors model tier, drains expiring quotas smoothly |
| `quality-first` | `2.5` | `1.8` | `0.6` | `0.2` | Keeps best coding model unless hard-throttled |
| `burn-expiring` | `1.0` | `1.5` | `2.5` | `0.2` | Aggressively drains coding plans with high positive `surplus` on cold boundaries |
| `strict-failover` | `0.0` | `1.0` | `0.0` | `0.1` | Pure deterministic target `priority` order + warm cache pin (`ToolLoop`/`NewUserTurn`) |

Explicit `policy.weights` override individual preset values. Candidates sort descending by `Score` (tie-broken by target `priority`, deterministic `plan/model` ID) to form the ordered **Failover Candidate List** for Stage 7.

#### 4.5 Explainable `RouteDecision` trace example

```json
{
  "routeId": "coding-main",
  "turnPhase": "NewUserTurn",
  "sessionKey": "ses_8f2a9c10",
  "demandTier": "strong",
  "semanticTierUsed": false,
  "cacheWarmAgeSec": 38,
  "cacheGravityMax": 0.87,
  "candidates": [
    {
      "plan": "kimi-coding",
      "model": "kimi-k2.5",
      "protocol": "anthropic",
      "nativeProtocol": true,
      "eligible": true,
      "scores": { "quality": 1.18, "cache": 1.74, "quota": -0.12, "protocol": 0.30, "health": 0.00, "total": 3.10 }
    },
    {
      "plan": "glm-coding",
      "model": "glm-4.7",
      "protocol": "anthropic",
      "nativeProtocol": true,
      "eligible": true,
      "scores": { "quality": 1.17, "cache": 0.00, "quota": 0.78, "protocol": 0.30, "health": 0.00, "total": 2.25 }
    }
  ]
}
```

---

## 7. Attempt execution, first-SSE-event peek, and failure classification

### 7.1 Request buffering and body limits

To replay an exchange across multiple failover candidates, the ingress handler buffers the incoming request body up to `DefaultMaxRequestBody` (`32 MiB`, configurable via `--gateway-max-request-body`). Requests exceeding the cap are either rejected cleanly with HTTP `413` (`invalid_request_error`) or, when `maxAttempts == 1`, streamed once without failover.

### 7.2 Pre-commit failover loop + First-SSE-Event Peek

Many third-party OpenAI/Anthropic compatible endpoints return **HTTP `200 OK` with `Content-Type: text/event-stream`** and then emit a rate-limit or quota error as their first SSE frame. Writing headers immediately to the client would prevent silent failover.

Execution sequence per attempt (up to `route.policy.maxAttempts`, default `3`):
1. Build upstream request (Fast Path patch or Bridge IR encode) with `timeout.ttft` (default `5m`) on response headers.
2. Execute `RoundTrip` on the plan's pooled `http.Transport` (honoring optional `plan.proxyUrl`).
3. Inspect HTTP status:
   - If non-`2xx`: read bounded error body (`<= 64 KiB`), classify error, apply cooldown if warranted, close upstream body, continue to next candidate.
4. If HTTP `2xx` and `Content-Type` contains `text/event-stream`:
   - **Peek** the stream (bounded by `timeout.ttft` remaining) skipping empty lines, `:keepalive` comments, and benign initial metadata frames (`message_start` without error) until the **first substantive event or data payload** arrives (max peek buffer `64 KiB`).
   - If the peek hits `EOF` with no events or parses an upstream error object (`{"type":"error", ...}` / `{"error": ...}` / `finish_reason: "error"`): classify as upstream failure, close upstream stream, and **fail over silently to the next candidate**.
5. **Commit point:** Write response status + headers (`X-Crabbro-*` trace headers included) to `http.ResponseWriter`, replay the peeked bytes verbatim (Fast Path) or emit translated initial events (Bridge Path), and stream remaining bytes with immediate `Flush()` while feeding `UsageTap`.
6. Update `SessionCacheStore` and insert one row into `usage.db` asynchronously via a bounded worker queue.

If an upstream stream drops **after** headers are already committed to the client (`downstream_stream` error), the executor records the failure on the trace, applies a soft health penalty/cooldown to the plan, and emits a protocol-formatted terminal error event on the stream so the client can cleanly retry on the next turn (which will route away from the degraded plan).

### 7.3 Error classification and cooldown matrix

| Failure Signal | Error Kind | Retry Next Candidate? | Cooldown Scope & Duration |
|---|---|---|---|
| `429 Too Many Requests` / SSE `rate_limit_error` | `rate_limit` | Yes | **Plan**; honor `Retry-After` / reset header, else exponential backoff (`15s`, `30s`, `60s`, max `5m`) |
| `402 Payment Required` / quota exhausted error | `quota_exhausted` | Yes | **Plan**; cooldown `5m` (or until next window reset if earlier) |
| `401` / `403` Auth / Invalid key | `auth_error` | Yes | **Plan**; long cooldown (`10m`), emits `gateway_plan_health_changed` |
| `404` Model not found / `model_not_found` | `model_unavailable` | Yes | **Plan+Model** pair only (`10m`); other models on same plan stay healthy |
| `400` Context length exceeded (`context_length_exceeded`) | `context_overflow` | Yes (only if a remaining candidate has larger `contextWindow`) | **None** |
| `400` / `422` Other client payload errors | `client_bad_request` | **No** (return verbatim/translated error to client) | **None** |
| `500` / `502` / `503` / `529` Overloaded / Dial / TTFT timeout | `upstream_unavailable` | Yes | **Plan**; short jittered cooldown (`10s` -> `30s`) |

### 7.4 Dedicated transport defaults

The gateway constructs its own transport pool distinct from `internal/proxy`:
- `ResponseHeaderTimeout`: `plan.timeout.ttft` (default `5m` for slow reasoning/thinking models).
- `Dialer.Timeout`: `10s`, `TLSHandshakeTimeout`: `10s`, `IdleConnTimeout`: `90s`.
- `Proxy`: `nil` (direct) unless `plan.proxyUrl` is explicitly set; plans sharing the same `proxyUrl` share a cached `*http.Transport`.
- Downstream `http.Server` sets `ReadHeaderTimeout: 30s` and `IdleTimeout: 120s`, with **no** `WriteTimeout` so long streaming generations are never aborted mid-response.

### 7.5 Provider reset coupons and opt-in auto-reset

Some coding plans (BigModel `zcode`) grant reset coupons that restore an exhausted window. Crabbro treats them as a provider-private resource owned by `internal/gateway/quota`:

- **Sync:** every quota refresh reads `GET /api/biz/customer-package-reset/list?targetType=PERSONAL`. Entries carry `recordId`, `grantType`, `available`, and `expireTime` — a naive `"2006-01-02 15:04:05"` wall clock in `Asia/Shanghai` (`+08:00`) that is parsed with a fixed zone and normalized to UTC. Snapshots expose only `scope`, `expiresAt`, `resetsRemaining`, and `soonestResetExpiry`; `recordId`/`grantType` never leave the package (no JSON output, log line, event, or UI field).
- **Explicit redemption:** `POST /v1/gateway/quotas/reset` (CLI `crabbro gateway quotas reset <plan> [--scope fiveHour|weekly] [--dry-run]`) re-reads `/list`, picks the soonest-expiring available coupon for the scope, redeems **at most one** coupon via `POST /api/biz/customer-package-reset/use` with a fresh UUID `requestId`, refreshes the plan quota snapshot, and clears plan cooling so traffic resumes immediately. An empty scope picks the soonest-expiring coupon across both windows.
- **Opt-in auto-redeem:** plans may declare

  ```yaml
  autoReset:
    enabled: true            # strictly opt-in; default false
    scopes: [fiveHour]       # default: both fiveHour and weekly
    expiringWithin: 24h      # only coupons expiring inside this horizon; empty = any
    minInterval: 5m          # anti-double-spend cooldown between auto redemptions
    safeguard:               # opt-in pre-expiry redemption; default off
      enabled: true
      leadTime: 30m          # redeem when the scope's coupon expires inside this lead
      minUsedPercent: 80     # ...and the same window is at least this used
  ```

  When enabled, the gateway redeems a coupon only after it observes the matching window exhausted (provider-reported usage `>= 99%`) — either from a quota refresh or from an upstream `quota_exhausted` / `rate_limit` classification (which re-fetches the windows first). The pre-expiry safeguard additionally redeems when the scope's soonest coupon is inside `leadTime` **and** that scope's window (`fiveHour` or `weekly`) is at least `minUsedPercent` used, so a weekly coupon is never spent merely to recover a five-hour window while weekly quota remains plentiful. Redemption is guarded by a per-plan single-flight mutex plus the per-`(plan, scope)` `minInterval`, always re-reads `/list` before `/use`, and **never blindly retries** an ambiguous or failed `/use`: the attempt fails closed to the normal cooling/failover path. A verified success refreshes the quota snapshot, clears plan cooling, and emits `gateway_quota_reset` carrying only the plan and scope.

- **Expiry reminders:** plans may configure `quotaReminder.leadTime` (default `24h`). When a coupon or an underutilized weekly/monthly window enters that horizon, the gateway emits one `gateway_quota_expiring` event per coupon/window (deduplicated by scope and expiry). The sanitized `reminders` list in the quota snapshot uses the same lead time; provider coupon identifiers never appear in either surface.

---

## 8. Configuration files, schemas, and snapshot lifecycle

Gateway configuration follows the exact per-file isolation and atomic commit model of [rules.md](./rules.md):

```text
~/.crabbro/gateway/
  usage.db
  plans/
    glm-coding.yaml
    glm-coding.yaml.draft    # unvalidated draft; ignored by live snapshot
    typesafe-jev.yaml
  routes/
    coding-main.yaml
    decision.yaml
```

Limits: 256 KiB max per YAML file; max 64 plans and 64 routes; symlinks and dotfiles are ignored with diagnostics; filename stem must equal top-level `id` (`^[a-z0-9][a-z0-9-]{0,63}$`).

### 8.1 Example configurations

#### Multi-endpoint Coding Plan (`~/.crabbro/gateway/plans/glm-coding.yaml`)

```yaml
version: 1
id: glm-coding
enabled: true
description: Zhipu GLM coding plan with shared quota across Anthropic and OpenAI endpoints
credential: glm_coding_key
timeout:
  ttft: 120s
  idle: 90s
cache:
  ttl: 5m
limits:
  rpm: 60
  windows:
    - name: five-hour
      duration: 5h
      maxTokens: 50000000
      resetSchedule: rolling
      mode: soft
    - name: weekly
      duration: 7d
      maxTokens: 400000000
      resetSchedule: rolling
      mode: hard
endpoints:
  anthropic:
    baseUrl: https://open.bigmodel.cn/api/anthropic
    apiVersion: "2023-06-01"
  openai-chat:
    baseUrl: https://open.bigmodel.cn/api/coding/paas/v4
models:
  glm-4.7:
    tier: strong
    codingScore: 0.85
    contextWindow: 200000
    maxOutputTokens: 16384
    capabilities:
      tools: true
      vision: false
      thinking: true
      structuredOutput: true
      promptCache: true
```

#### TypeSafe Jev Plan (`~/.crabbro/gateway/plans/typesafe-jev.yaml`)

```yaml
version: 1
id: typesafe-jev
enabled: true
description: TypeSafe System One (Jev) calibrated decision backend
credential: typesafe_api_key
timeout:
  ttft: 5s
  idle: 10s
limits:
  rpm: 300
endpoints:
  typesafe-jev:
    baseUrl: https://api.typesafe.ai
models:
  jev-latest:
    tier: fast
    codingScore: 0.50
    contextWindow: 65536
    capabilities:
      structuredOutput: true
```

#### Generative Coding Route (`~/.crabbro/gateway/routes/coding-main.yaml`)

```yaml
version: 1
id: coding-main
enabled: true
description: Primary coding agent virtual route across API-key plans
priority: 100
family: generative
match:
  protocols:
    - anthropic
    - openai-chat
    - openai-responses
    - gemini
  models:
    - claude-sonnet-*
    - coding-main
policy:
  preset: balanced
  preferNativeProtocol: true
  allowCrossProtocol: true
  maxAttempts: 4
  semanticTiering:
    enabled: true
    decisionRoute: decision
    timeout: 800ms
    minConfidence: 0.72
    maxCacheGravity: 0.35
targets:
  - plan: kimi-coding
    model: kimi-k2.5
    priority: 10
    weight: 10
    enabled: true
    whenTier:
      - strong
      - frontier
  - plan: glm-coding
    model: glm-4.7
    priority: 20
    weight: 10
    enabled: true
    whenTier:
      - fast
      - strong
  - plan: deepseek-direct
    model: deepseek-chat
    priority: 30
    weight: 5
    enabled: true
```

#### Decision Route (`~/.crabbro/gateway/routes/decision.yaml`)

```yaml
version: 1
id: decision
enabled: true
description: Default decision route with native Jev primary and Gemini/OpenAI emulation fallback
priority: 10
family: decision
match:
  protocols:
    - typesafe-jev
  models:
    - "*"
policy:
  preset: strict-failover
  preferNativeProtocol: true
  allowEmulation: true
  maxAttempts: 3
targets:
  - plan: typesafe-jev
    model: jev-latest
    priority: 10
    enabled: true
  - plan: gemini-api
    model: gemini-2.5-flash
    endpointProtocol: gemini
    priority: 20
    enabled: true
  - plan: openai-api
    model: gpt-4o-mini
    endpointProtocol: openai-chat
    priority: 30
    enabled: true
```

### 8.2 Cross-file validation and atomic Snapshot reload

`internal/gateway/config` validates:
1. JSON Schema Draft 2020-12 (`gateway-plan.v1.json`, `gateway-route.v1.json`).
2. Cross-file integrity: every `target.plan` resolves to a loaded plan, `target.model` exists in `plan.models`, optional `target.endpointProtocol` exists in `plan.endpoints`, semantic family constraints hold (`generative` routes never target plans that only expose `typesafe-jev`; `decision` routes targeting generative endpoints require `allowEmulation: true`), and when `semanticTiering.enabled` is true, `semanticTiering.decisionRoute` must resolve to an explicit loaded route with `family: decision`.
3. **Last-known-good retention:** If a live file fails reload after a manual disk edit, the gateway retains the previous valid `Snapshot`, emits a diagnostic on `/v1/config/status` and `/v1/gateway/status`, and never takes down active traffic. `config commit` renames `.draft` atomically and swaps `atomic.Pointer[Snapshot]`; in-flight requests keep the snapshot they entered with.

---

## 9. Control API, CLI, and desktop UI

### 9.1 Control API extensions

Reuse the existing config staging flow in `internal/api/server.go`:
- `GET /v1/config/status` — reports rules, scope lists, and gateway plans/routes + diagnostics.
- `POST /v1/config/{draft,validate,commit,discard}?kind=gateway-plan|gateway-route&id=<id>`

Add secret and gateway control endpoints:

| Method | Path | Purpose |
|---|---|---|
| `GET` / `POST` / `DELETE` | `/v1/secrets` & `/v1/secrets/{name}` | List secret names/metadata (never values), write (`0600`), delete |
| `GET` | `/v1/gateway/status` | Listener address, active snapshot generation, route/plan counts, health summary |
| `GET` | `/v1/gateway/plans` | Plan configurations (redacted), current cooldown state, rolling window consumption |
| `POST` | `/v1/gateway/quotas/refresh` | Re-sync provider quota snapshots (balance, windows, reset coupons) |
| `POST` | `/v1/gateway/quotas/reset` | Redeem at most one provider reset coupon: `{"plan":"zcode","scope":"fiveHour"\|"weekly","dryRun":bool}`. Picks the soonest-expiring available coupon from a fresh `/list` read, redeems it once with a unique `requestId`, refreshes the plan quota snapshot, and clears plan cooling. Provider coupon identifiers (`recordId`, `grantType`) never appear in responses, logs, or events |
| `POST` | `/v1/gateway/plans/{id}/reset-cooldown` | Clear active cooldown on a plan immediately |
| `GET` | `/v1/gateway/routes` | Loaded routes and match precedence |
| `GET` | `/v1/gateway/usage?window=5h&plan=...` | Aggregated tokens, cache hit ratios, error counts from `usage.db` |
| `POST` | `/v1/gateway/preview` | Dry-run router scoring (`profile`, `protocol`, `model`, `estimatedInputTokens`, `turnPhase`, optional `sessionKey`) without sending upstream bytes |
| `POST` | `/v1/gateway/reload` | Reload plans and routes from disk into a new `Snapshot` |

### 9.2 CLI commands

JSON output by default; `--pretty` for operator tables:
- `crabbro secrets set <name>` (reads from stdin or `--from-env VAR`, never a plain CLI value flag in `ps`), `secrets list`, `secrets delete <name>`.
- `crabbro gateway status`, `crabbro gateway plans`, `crabbro gateway routes`, `crabbro gateway usage [--window 5h|24h|7d]`, `crabbro gateway quotas refresh`, `crabbro gateway quotas reset <plan> [--scope fiveHour|weekly] [--dry-run]`, `crabbro gateway cooldown-clear <plan>`, `crabbro gateway preview --protocol anthropic --model coding-main --tokens 42000`.
- `crabbro config draft|validate|commit|discard --gateway-plan <id>` / `--gateway-route <id>`.
- `crabbrod` startup flags: `--gateway-addr` (default `127.0.0.1:8081`, `"off"` disables), `--gateway-token`, `--gateway-capture` (`preview | metadata-only | off`, default `preview`), `--gateway-max-request-body` (default `32MiB`), `--gateway-usage-retention` (default `90d`).

### 9.3 Desktop UI (`cmd/crabbro-ui`)

Using only `pkg/api` and `pkg/client` (no `internal/` imports):
- **Requests tab:** Add a filter pill (`All | Proxy | Gateway`); when selecting a gateway exchange, display the route badge, chosen `(plan, model)`, protocol translation indicator (`native` vs `translated` vs `emulated`), per-attempt timeline, token/cache breakdown, and `RouteDecision` candidate scores.
- **Gateway tab:** Live plan cards showing rolling window progress bars (`remainingRatio` vs `timeLeftRatio` pace marker), active cooldown countdown + manual Reset button, recent failover events, and active session sticky pins.

---

## 10. Security and privacy boundaries

- **Loopback by default:** Gateway listens on `127.0.0.1:8081`. Binding a non-loopback address requires an explicit gateway auth token (`--gateway-token`).
- **Strict key separation:** Clients send a local placeholder or gateway token (`ANTHROPIC_API_KEY=crabbro` or `--gateway-token`); upstream plan keys are injected only inside `executor` immediately before dialing upstream.
- **Scrubbing before capture:** Every captured request/response preview, trace, and SSE event passes through `secret.Scrubber` (headers, resolved credential strings, and Gemini `?key=` query strings).
- **Capture minimization:** `--gateway-capture metadata-only` records headers, token counts, and `RouteDecision` without storing prompt/completion bodies in `RingBuffer` or `history.db`, suitable for sensitive codebases.

---

## 11. Phased implementation roadmap

### Phase 1: Shared Core Refactoring
- Extract `internal/secret` (global `~/.crabbro/secrets/`, `Scrubber`), and update `internal/plugin` to take `*secret.Store` directly.
- Extract `ExchangeSink`, replace direct capture-store writes on `internal/proxy.Proxy` with `ExchangeSink`, set `Kind` explicitly to `"proxy"` or `"gateway"` on `pkg/api/request.go`, add `Gateway *GatewayTrace`, and add gateway SSE events to `pkg/api/event.go`.
- Add `/v1/secrets` control endpoints and `crabbro secrets` CLI.
- **Exit criteria:** `go test ./...` passes with zero behavioral regressions in forward proxy, rules, plugins, or UI.

### Phase 2: Gateway MVP (Native Passthrough + Unified Router + Ledger)
- Implement `internal/gateway/config` (YAML loader + Draft 2020-12 validation + `Snapshot` + `/v1/config/*?kind=gateway-plan|gateway-route`).
- Implement `internal/gateway/ledger` (`usage.db` SQLite store + rolling window + rate-limit header calibration).
- Implement `internal/gateway/router` (4-stage `SignalExtractor`, hard filters, `Quality × Cache × Quota` scorer, `SessionCacheStore`, `preview`).
- Implement `internal/gateway/executor` (32 MiB request buffer, pre-commit retry loop, **First-SSE-Event Peek**, cooldowns, per-plan `proxyUrl` transports) and same-protocol Fast Path + `UsageTap` for `anthropic`, `openai-chat`, `openai-responses`, `gemini`, `typesafe-jev`, and `openai-decisions`.
- **Exit criteria:** Multi-plan failover, fake-200 SSE peek failover, cache-preserving sticky routing, and `crabbro requests show` / `crabbro gateway usage` verified end-to-end with mock upstreams.

### Phase 3: Cross-Protocol Bridges and `internal/decision` Unification
- Implement `internal/gateway/ir` (`GenIR` + `DecisionIR`) and full bidirectional codecs for `openai-chat`, `openai-responses`, `anthropic`, and `gemini`, plus one-way `DecisionIR -> GenIR` emulation and thinking-signature isolation.
- Golden SSE fixture tests across all cross-protocol streaming tool-call and thinking scenarios.
- Wire cold-boundary `semanticTiering` (`hopDepth` guard), refactor `internal/decision` (`Find`, `Draft`, `Ask`) onto the gateway `decision` route with updated YAML v1 rule draft prompts, and delete direct provider HTTP callers, `FromEnv()`, and `--decision-*` flags.
- **Exit criteria:** Cross-protocol streaming tool loops (e.g., Claude Code on Anthropic ingress backed by OpenAI/Gemini plan) and `crabbro find` / `rules draft` via Jev/Gemini/OpenAI/Anthropic pass e2e tests.

### Phase 4: Desktop UI and Polish
- Add `pkg/client` typed gateway and secret methods.
- Extend `cmd/crabbro-ui` Requests detail view and add the Gateway tab.
- **Exit criteria:** `cmd/crabbro-ui` screenshot tests and full `go test ./...` green.

### Direct plan addressing

Route resolution considers, in order: an explicit route override, matching non-catch-all route patterns, a direct `<plan>/<model>` address, and finally catch-all routes. A known plan fails closed when disabled, direct access is disabled or restricted, the model is undeclared or ambiguous, or no compatible endpoint is available. Direct requests use the ordinary candidate filtering and scoring pipeline. The route decision records the selected tier in `resolution`.

`direct.enabled` defaults to true; `direct.profiles` restricts direct access when nonempty. A model may declare `endpoints` to restrict the plan protocols that serve it; omission inherits all plan endpoints. Literal model names are resolved first. Recognized trailing context annotations such as `[1m]` and reasoning qualifiers such as `:high` are then stripped iteratively, followed by a unique case-insensitive lookup. Traces retain the original request while upstream calls use the canonical model key.
