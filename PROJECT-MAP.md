# Crabbro Project Map

- Status: current
- Last verified: `3a8f3bb` / 2026-11-21
- Maintainer: crabbro
- Strategic health: Watch — forward HTTP/MITM proxy, YAML pipelines, MyGo desktop UI, and multi-protocol LLM/Decision gateway are implemented; browser-extension attribution and >256 KiB response rewrites remain out of tree.
- Purpose: Help technical decision-makers judge architectural integrity and project health; no daily todo lists or unverified claims.

> Detailed contracts live in [README.md](README.md) and [docs/design/](docs/design/gateway.md).

## 1. Strategic Goals

- Problem: Agents and developers need an inspectable, programmable local HTTP proxy and unified multi-provider LLM/Decision gateway without mutating third-party CLI configurations.
- Target state: One cohesive local daemon (`crabbrod`) serving a CLI (`crabbro`) and native desktop UI (`crabbro-ui`), cleanly separating forward proxy capture/rules (`:8080`) from the LLM & Decision gateway (`:8081`) over shared core infrastructure.
- Non-goals: OAuth/cookie scraping for consumer web subscriptions, auto-editing external configs (`~/.claude/settings.json`), non-text media modalities, or running forward-proxy rewrite rules over gateway SSE streams.
- Success criteria: Captured flows and gateway attempts are deterministically inspectable, replayable, secret-scrubbed, and explainable (`crabbro gw preview`, `verify replay`).

## 2. Architecture

```text
CLI (cmd/crabbro) / Native MyGo Desktop UI (cmd/crabbro-ui)
                 ↕  /v1 control API (Unix socket 0600 / loopback TCP)
                       crabbrod (cmd/crabbrod)
   ┌──────────────────────────┴──────────────────────────┐
Forward Proxy (:8080)                     LLM & Decision Gateway (:8081)
HTTP / CONNECT / MITM / WS                6 codecs + Fast/IR paths + Router
YAML pipelines + Goja fallback            SSE pre-commit peek + failover
   └──────────────────────────┬──────────────────────────┘
                        Shared Core Layer
internal/secret | ExchangeSink (ring/history.db) | usage.db | internal/decision | config staging
```

| Part | Responsibility | Boundary / Non-responsibility | Key Entry | Status |
| --- | --- | --- | --- | --- |
| Daemon & Control API | Host proxy, gateway, sessions, MITM, `/v1` API & SSE events | Does not edit client CLI config files | `cmd/crabbrod`, `internal/api` | Available |
| Forward Proxy & Rules | HTTP/HTTPS MITM, WebSocket frames, YAML `uses` pipelines + Goja fallback | Bodies >256 KiB forwarded unchanged; no WS over HTTP/2 | `internal/proxy`, `internal/rule` | Available |
| LLM & Decision Gateway | `openai-chat`, `openai-responses`, `anthropic`, `gemini`, `typesafe-jev`, `openai-decisions`; `Quality x Cache x Quota` routing; `usage.db` | No audio/image/embedding modalities; no USD billing claims | `internal/gateway`, `docs/design/gateway.md` | Available |
| Node Plugin Host | Lazy Node host with per-plugin `worker_threads` for npm rule plugins | Not an OS sandbox; `crabbro.*` builtins reserved | `internal/plugin`, `cmd/crabbro-plugin-host`, `sdk/plugin` | Available |
| Clients (CLI & Desktop UI) | Typed `/v1` JSON consumer, MyGo native 4-page app (`Crabbro.app`) | Neither `pkg/api` nor `pkg/client` imports a UI toolkit | `cmd/crabbro`, `cmd/crabbro-ui`, `pkg/client` | Available |

## 3. Primary Work

- Maintain and harden the shared core + peer proxy/gateway data planes in `crabbrod`.
- Provide agent-first JSON CLI ergonomics (`requests`, `find`, `rules from/draft/test`, `verify replay`, `gw status/preview/usage`, `config draft/validate/commit`).
- Keep the MyGo native desktop UI aligned with the `/v1` snapshot-then-follow contract.

## 4. Work Methods

- Contracts: Designs live in `docs/design/` ([gateway.md](docs/design/gateway.md), [rules.md](docs/design/rules.md), [plugins.md](docs/design/plugins.md), [ui-api.md](docs/design/ui-api.md), [inspect.md](docs/design/inspect.md)); schemas live in `schemas/`.
- Verification: `go vet ./...` and `go test ./...` (`go 1.27.3`), rule fixture checks, and local `scripts/install.sh` bundle verification.
- Language & Git conventions: Strictly English across all repository files, code, docs, UI copy, and commits; maintain `AGENTS.md` + `CLAUDE.md` (`@AGENTS.md`) + `PROJECT-MAP.md` triad; linear `main` history; cross-session tasks recorded in `~/.workcells` (`task/local`) without secrets.

## 5. Core Mechanisms & Commands

- **Zero-leak secrets**: Mode-`0600` credentials in `~/.crabbro/secrets/` are referenced by name in YAML plans/routes and scrubbed before capture or logging.
- **Safe config staging**: Rules, scope, `--gateway-plan`, and `--gateway-route` use atomic per-file `draft/validate/commit/discard`, keeping last-known-good state on broken edits.
- **Two stores**: Exchange captures persist to `~/.crabbro/history.db` (7-day / 10,000-row retention); gateway quota rolling windows persist separately in `~/.crabbro/gateway/usage.db` (non-pruned).

| Command | Purpose | Status | Reference |
| --- | --- | --- | --- |
| `crabbro daemon start/stop`, `status`, `doctor` | Daemon lifecycle and environment checks | Available | [README.md](README.md#cli-reference) |
| `crabbro record`, `requests`, `find`, `verify replay` | Traffic capture, natural-language query, and replay verification | Available | [README.md](README.md#agent-workflow) |
| `crabbro rules`, `config`, `mitm` | Declarative YAML rules, atomic config commits, MITM CA | Available | [docs/design/rules.md](docs/design/rules.md) |
| `crabbro gw`, `secrets` | Multi-protocol LLM/Decision gateway routing, quotas, secrets | Available | [docs/design/gateway.md](docs/design/gateway.md) |
| `scripts/install.sh` | Build CLI, daemon, and `Crabbro.app` (macOS) / CLI (Linux) | Available | [scripts/install.sh](scripts/install.sh) |

## 6. Changes, Risks & Health

- Recent changes: Renamed from Chiling to Crabbro (`dc469ae`, 2026-11-19) and landed the multi-protocol LLM & Decision Gateway with shared core refactoring (`3a8f3bb`, 2026-11-20).
- Drift risk: Adding proxy rewrite features to gateway streams or bypassing atomic config validation would collapse the clean two-plane architecture.
- Known gaps: Browser-extension tab/initiator attribution beyond headers; >256 KiB response body rewrites; live trust-store detection; WebSocket message reassembly and WebSocket over HTTP/2.
- Verdict: **Watch**. Core architecture is cohesive after the gateway refactoring; keep `inspect.md` scope boundaries and zero-leak invariants tight.

## 7. Further Reading

- Overview & CLI: [README.md](README.md)
- Subsystems: [docs/design/gateway.md](docs/design/gateway.md), [docs/design/rules.md](docs/design/rules.md), [docs/design/plugins.md](docs/design/plugins.md), [docs/design/ui-api.md](docs/design/ui-api.md), [docs/design/inspect.md](docs/design/inspect.md)
