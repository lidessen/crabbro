# UI control API

The desktop UI is a client of `chilingd`. It is not a second proxy, and it does not link the daemon into the window process. Closing the window leaves capture, rules, and MITM running.

The wire contract is HTTP `/v1` over the owner-only Unix socket (`~/.chiling/proxy.sock`) and, as a fallback, `localhost:16379` with the bearer token in `~/.chiling/api.token`. The CLI, agents, and the UI all use that contract. JSON objects only gain fields. Renaming or removing a field is a new major version (`/v2`), not a silent change.

Go types live in `pkg/api`. `pkg/client` is the typed caller. Neither package imports a UI toolkit, and neither imports `internal/`, so a later module (Go 1.27, for a [MyGo](https://mygo.egoist.dev/docs/ui) native view) can depend on them. The daemon keeps compiling against the same structs through aliases in `internal/shared`, `internal/inspect`, `internal/plugin`, and `internal/history`.

This tree does not contain the view. A MyGo view is an immediate-mode function of app state, called every frame, and it may read state that another goroutine updates.

## How the view should consume it

1. Call `Subscribe` first and buffer events on a goroutine.
2. Snapshot what the screen needs: `Status`, `Scope`, `Activity` (follow `NextCursor` until it is empty, or keep only the newest page you will draw), `ListRequests`, and `ListRules` if that pane is open.
3. Apply buffered events, then keep applying live ones. Drop an event whose request id or activity id is already in the snapshot so a step that arrived mid-snapshot is not inserted twice.
4. The goroutine writes the state the view reads. It does not call into the view. The frame function only reads that state and calls client methods for clicks (pick, enable, replay).

`Subscribe` is live-only on the first connection. A dropped stream reconnects with `?after=` set to the last event id the client saw. The daemon replays from a 512-event ring, then continues live. A gap longer than that ring is filled by taking the snapshot again.

## Client methods

| Method | Request |
|---|---|
| `Status` | `GET /v1/status` |
| `Activity`, `Trajectory` | `GET /v1/trajectory?session=&cursor=&limit=` |
| `ListRequests` | `GET /v1/requests` |
| `SearchRequests` | `GET /v1/requests?q=` |
| `GetRequest` | `GET /v1/requests/{id}?include=headers,body,rule-trace` |
| `ListSources` | `GET /v1/sources` |
| `GetSource` | `GET /v1/sources/{id}` |
| `Scope` | `GET /v1/scope` |
| `ScopePick`, `ScopeOmit`, `ScopeOff`, `ScopeOn`, `ScopeReset`, `ScopeBypass` | `POST /v1/scope` `{"op","expr"}` |
| `ListRules` | `GET /v1/rules` |
| `GetRule` | `GET /v1/rules/{id}` |
| `PreviewRules` | `POST /v1/rules/preview` |
| `ValidateRule` | `POST /v1/config/validate?id=` |
| `DraftRule` | `POST /v1/config/draft?id=` |
| `CommitRule` | `POST /v1/config/commit?id=` |
| `DiscardRule` | `POST /v1/config/discard?id=` |
| `EnableRule`, `DisableRule` | `POST /v1/rules/{id}/toggle` |
| `ListPlugins` | `GET /v1/plugins` |
| `InspectPlugin` | `GET /v1/plugins/{id}` |
| `PluginTools` | `GET /v1/plugins/{id}/tools` |
| `InvokePlugin` | `POST /v1/plugins/{id}/instances/{instance}/tools/{tool}` |
| `ListSessions` | `GET /v1/sessions` |
| `CurrentSession` | `GET /v1/sessions/current` |
| `StartRecording` | `POST /v1/sessions` |
| `StopRecording` | `POST /v1/sessions/{id}/stop` |
| `Replay`, `Verify` | `POST /v1/verify/replay` |
| `History` | `GET /v1/history` |
| `CAStatus` | `GET /v1/mitm/status` |
| `Subscribe` | `GET /v1/events` |
| `EnsureDaemon` | starts `chilingd` when `/v1/status` does not answer |

`Do` is the escape hatch the CLI uses for routes that do not have a method yet. `EnsureDaemon` starts the daemon with default flags only. `chiling daemon start` is still how you choose the proxy address and MITM.

`ListRequests` includes `hiddenCount` (how many exchanges the current scope removed) and `scopeActive`. Pass `RequestQuery.All` to keep hidden rows. `GetRequest` with `Include.Body` fills `body` and `responseBody` when the daemon still has the bytes.

`GET /v1/activity?session=&limit=` remains the tail of the timeline. The paged screen should use `/v1/trajectory`.

## Trajectory

`GET /v1/trajectory` returns oldest-first steps:

```json
{"items":[{"id":"act_1","at":"...","kind":"session_started","sessionId":"ses_1","summary":"..."}],"nextCursor":"act_1"}
```

`cursor` is the previous page's `nextCursor` (an `act_N` id). An empty cursor starts at the oldest retained step. `limit` defaults to 50 and is capped at 200. An empty `nextCursor` means there is no further page. A cursor that is not `act_N` is `400`.

The log is capped (1024 steps in the daemon). Kinds the screen should expect:

| Kind | When |
|---|---|
| `session_started`, `session_stopped` | recording window |
| `request_captured`, `request_completed` | a request while a session is recording. `requestId` links to `GET /v1/requests/{id}` |
| `rule_applied` | a rule matched on the request or response phase. `ruleId` is set |
| `rule_created` | a rule was created from the API |
| `verify` | a replay finished |
| `find` | a decision search |
| `scope_picked`, `scope_omitted`, `scope_restored`, `scope_reset`, `scope_bypass`, `scope_committed` | session or durable scope edits |

The request list is still the full flow table. The trajectory is the narrative (sessions, captures while recording, rule applies, verifies, agent actions).

## Events

`GET /v1/events` is `text/event-stream`. Each event is:

```text
id: 4
data: {"id":4,"type":"scope_changed","requestId":"","timestamp":"...","payload":{...}}
```

Resume with `?after=4` or the `Last-Event-ID: 4` header. Events with `id` less than or equal to that value are not replayed. Repeat `type` (or pass a comma-separated list) to keep a subset; the client filter does the same.

| `type` | Meaning | `payload` |
|---|---|---|
| `request_started` | exchange accepted | `method`, `url`, `host`, `sessionId`, `hidden` when known |
| `request_headers` | request headers stored | |
| `request_body_available` | request body stored | |
| `rule_evaluated` | one rule ran | `ruleId`, `matched`, optional `error` |
| `rule_applied` | a matched rule produced actions | `ruleId`, `actions`, optional `phase` |
| `response_headers` | response headers stored | |
| `response_body_available` | response body stored | |
| `response_completed` | exchange finished | `status`, optional `stage` |
| `request_failed` | exchange failed | optional `stage` |
| `mitm_enabled`, `mitm_disabled` | operator toggled MITM | |
| `scope_changed` | scope pick, omit, bypass, reset, or commit | `ScopeState` |
| `activity` | a trajectory step was appended | `Activity` |

`requestId` on request and rule events is the exchange to refetch or to patch. `activity` can be appended to the timeline. `scope_changed` replaces the scope snapshot. Slow subscribers still drop live events; the ring plus `after` covers a short reconnect, and the list endpoints cover the rest.

## Versioning

`pkg/api.APIVersion` is `v1`. Paths are `/v1/...`. Clients must ignore unknown JSON fields. The daemon and `pkg/client` share the structs in `pkg/api`, so a field cannot drift between them.

## What is deferred

- The MyGo view itself, and a separate module that needs Go 1.27.
- MyGo's own packaging (`mygo.json`, signing, notarization, auto-update). That should replace `scripts/install.sh` for the `.app` once the native UI exists. The script stays the installer until then, and this change does not modify it.
- A TypeScript client. The UI calls Go directly.
- Plugin inspect and invoke results stay free-form JSON because the host returns that.
- Browser-extension or DevTools attribution. Header-derived sources are what `ListSources` returns today.
- An event ring longer than 512, or a durable event log. After a long disconnect, snapshot again.
