# Crabbro Development Entry Point

Start with [README.md](README.md) for CLI/gateway usage and [PROJECT-MAP.md](PROJECT-MAP.md) for strategic architecture and health. `CLAUDE.md` delegates here via `@AGENTS.md`.

## Architecture & Invariants

- **Two peer data planes**: `internal/proxy` (`:8080`, forward HTTP/MITM/WS + YAML rule pipelines) and `internal/gateway` (`:8081`, 5 wire codecs + Fast/IR paths + `Quality x Cache x Quota` router) share core components (`internal/secret`, `ExchangeSink`, `usage.db`, `internal/decision`, atomic config lifecycle) without mixing proxy response rewrites into gateway SSE streams.
- **Zero-leak secrets**: Secrets live mode `0600` in `~/.crabbro/secrets/` (`internal/secret`) and are scrubbed from headers, bodies, SSE events, history, and query parameters. Never write plaintext credentials into YAML files, logs, tests, or Workcells records.
- **Never mutate external agent configs**: `crabbrod` never edits `~/.claude/settings.json`, `~/.codex/config.toml`, or other third-party CLI configuration files.
- **Client packages stay UI-free**: `pkg/api` and `pkg/client` hold `/v1` wire types and control-socket transport only; neither may import a UI toolkit.
- **Per-file config isolation**: Rules, scope lists, `--gateway-plan`, and `--gateway-route` use `draft`/`validate`/`commit`/`discard` atomic swaps with Draft 2020-12 JSON schemas in `schemas/`.

## Common Commands

```sh
go vet ./...
go test ./...
go run ./cmd/crabbro-ui
./scripts/install.sh
```

Go version is `1.27.3` (`go.mod`). Note `scripts/install.sh` updates `/Applications/Crabbro.app` (unless `--app-dir` is given) and `~/.local/bin/crabbro`; use `--dry-run` when verifying packaging without installing globally.

## Conventions

- **English only**: Keep all repository prose, documentation, `AGENTS.md`, `PROJECT-MAP.md`, comments, UI text, tests, and commit messages in English.
- **Git hygiene**: Keep linear history on `main` (rebase / fast-forward); clean up temporary worktrees and merged branches; never commit local debug logs or machine-local home paths.
- **Verification discipline**: Separate static checks (`go vet`), unit/integration tests (`go test`), installed-app checks, and live proxy/gateway runs; report unverified paths honestly.
- **Cross-session collaboration**: For multi-session or cross-project work, record Tasks and immutable Handoffs in `~/.workcells` (`task/local` record mode, without `task start`), and archive completed task records in `~/.workcells/workspace/`.
