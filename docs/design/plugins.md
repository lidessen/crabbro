# Chiling Plugin Design

Status: Proposal, not implemented
Date: 2026-11-04
Scope: JS plugins distributed as npm packages. This does not change the current rule sandbox.

`NETWORK_PIPELINE_DESIGN.md` decision 7 kept plugin installation, manifests, host capabilities, and command registration out of the network work. This document is that follow-on. Rules stay what they are today: one JavaScript function, evaluated by Goja, returning a validated action list.

## 1. Decision summary

1. Plugins are npm packages. The daemon does not execute them inside Goja. Goja stays the rule engine.
2. Each enabled plugin is a long-lived Node process started by `chilingd`. Hooks are JSON messages over a private socket. The process is started once, not once per request.
3. A plugin returns the same declarative actions rules return. Go validates and applies them. Plugin code never receives `http.Request`, a socket, or the response writer.
4. Built-in action names are reserved. A plugin action is `plugin.<package>.<action>` and cannot replace `mockResponse` or any other built-in.
5. A rule may emit a plugin action. The rule still matches in Goja. Only that action's handler crosses into Node.
6. Default permissions are none: no network, no filesystem outside the plugin directory, no environment, no history writes. Native addons and install scripts are refused unless the operator opts in.
7. CLI commands a plugin registers live under `chiling plugin run <pkg> ...`. They do not add top-level `chiling` subcommands.
8. Install, enable, and traces go through the daemon. The CLI is a client. Output is JSON. A UI later reads the same API and the same activity log.

## 2. What exists today

`internal/rule.RuleEngine` builds a new Goja VM per evaluation (`evaluate` in `engine.go`). The VM has no filesystem, network, process, or environment. `console.log` is a no-op. `export default` is rewritten into a `var __rule__` assignment because Goja does not load ES modules or `node_modules`. Each call has a 500 ms timeout (`DefaultRuleTimeout`) and the exchange has a 2 s budget (`DefaultRuleBudget`). The serialized result is capped at 64 KiB. `ValidateOutput` rejects the whole result if any action is invalid.

The proxy compiles enabled rules into one onion (`ruleMiddleware` in `internal/proxy`). Request actions run on the way in. Response actions (`setResponseHeader`, `removeResponseHeader`, `setResponseStatus`, `rewriteResponseBody`, `delay`, `throttle`) run on the way out. If the request-phase result matched and contained no response actions, the same script is called again with `ctx.response`. WebSocket data frames are offered only to rules that already matched the upgrade, as `ctx.frame`. Ping, pong, and close are not offered.

Actions the daemon already knows how to apply:

| Stage | Actions |
|---|---|
| Request | `tag`, `setRequestHeader`, `removeRequestHeader`, `rewriteURL`, `mockResponse`, `block`, `abort` |
| Response | `setResponseHeader`, `removeResponseHeader`, `setResponseStatus`, `rewriteResponseBody`, `delay`, `throttle` |
| WebSocket data frame | `replaceFrame`, `dropFrame` |

Rules are an in-memory map (`RuleStore`). They are lost when the daemon exits. Captured exchanges are not: `internal/history` writes them to SQLite. The control API is `/v1` over an owner-only Unix socket, with a bearer token on TCP. The CLI defaults to JSON. `internal/session` records a trajectory (`request_captured`, `rule_created`, `verify`, `find`) for a later UI.

That split is the constraint this proposal keeps. A hot-path rule must not grow a Node dependency. A distributed plugin must not be squeezed into a runtime that cannot load one.

## 3. Goals and non-goals

### Goals

- An operator or agent can install a plugin from npm and enable it without editing the daemon.
- Plugin authors write TypeScript against a types-only package and ship compiled JavaScript.
- Hooks line up with the stages the proxy already has: request, response, WebSocket frame.
- Anything a plugin returns is checked by the same validator as a rule, then applied by Go.
- Agents can list plugins, read their manifests, and call their commands from JSON.
- A UI can show install, enable, and per-exchange plugin traces without a second control plane.

### Non-goals for the first implementation

- Running untrusted anonymous code. Installing a package is an operator decision. Permissions reduce accidents and surprise install scripts. They are not a sandbox against a hostile native addon.
- Replacing rules with plugins. One-off rewrites stay inline JavaScript.
- Loading plugins from the network on each request.
- A plugin store or search index inside chiling. npm is the index.
- Plugin-owned MITM policy. A plugin match must not decrypt a host. That stays `NetworkPolicy`.
- Editing history or deleting captures in the first permission set.

## 4. Runtime

### Options

| | Goja, same sandbox as rules | Node subprocess | Bun subprocess |
|---|---|---|---|
| npm dependencies | No. Goja does not resolve `node_modules`, and the current preprocessor only rewrites a leading `export default`. | Yes. `npm install` is the distribution path the owner asked for. | Yes, with a smaller compatibility gap than Goja and a larger one than current Node. |
| Cold start | None. The VM is created per evaluation and is cheap. | A process start is too slow to do per request. A process kept for the daemon lifetime is fine. | Faster startup than Node, same "keep it alive" requirement. |
| Isolation | Best of the three for pure JS. A timeout interrupts the VM. A script cannot touch the disk. | A hung plugin can be killed without exiting `chilingd`. Node is not a security boundary once native code or `child_process` is reachable. | Same process story as Node. |
| Author experience | Authors could not `import` a normal package. The types they write would not match the runtime. | Matches how npm packages are written and tested. | Authors would still test on Node, because that is what CI and editors run. |

### Recommendation

Use Node as the plugin host. Keep Goja for rules.

One OS process per enabled plugin, started when the plugin is enabled and stopped when it is disabled or the daemon shuts down. A shared Node process for every plugin would use less memory and is a reasonable later optimization. Separate processes make a timeout a kill, and they stop one package from reading another's config. A local proxy is expected to have a few plugins, not hundreds.

The host program is a small script shipped with chiling (`internal/pluginhost` or `cmd/chiling-plugin-host`). The daemon speaks length-prefixed JSON frames over a Unix socket created mode `0600` in the plugin state directory. Frames are `load`, `hook`, `action`, `command`, and `shutdown`. The daemon owns the timeout: if a hook exceeds its budget, the daemon kills that process, records the error on the exchange, and follows the plugin's failure mode (default fail-open, same as rules). The next exchange starts a fresh process.

Bun is not the v1 host. It can be a later `chiling plugin host --runtime bun` experiment if startup or install speed becomes the problem. The package format should not mention the runtime, so that swap stays possible.

Do not try to teach Goja to load npm. A partial resolver would accept packages that then fail on `import`, top-level await, or a dependency. That is a worse author experience than a boring Node process.

## 5. Package shape

A plugin is a normal npm package whose `package.json` carries a `chiling` block. The daemon reads that block as data. It does not execute `package.json`.

```json
{
  "name": "@example/chiling-redact",
  "version": "1.2.0",
  "type": "module",
  "exports": {
    ".": "./dist/index.js"
  },
  "chiling": {
    "api": "1.0.0",
    "entry": "./dist/index.js",
    "permissions": ["history:read"],
    "configSchema": "./chiling.config.schema.json"
  }
}
```

| Field | Required | Meaning |
|---|---|---|
| `chiling.api` | yes | Semver of the plugin API this package was built against. The daemon loads it only when the major matches. |
| `chiling.entry` | yes | JavaScript file, relative to the package root, no `..`. |
| `chiling.permissions` | no | Declared capabilities. Default is empty. See section 8. |
| `chiling.configSchema` | no | JSON Schema for the plugin's config object. |

The entry module default-exports one object:

```ts
import type { Plugin } from "@chiling/plugin";

const plugin: Plugin = {
  match: { hosts: ["*.example.com"], methods: ["POST"] },
  hooks: {
    onRequest(ctx) { return { match: false }; },
    onResponse(ctx) { return { match: true, actions: [/* built-in or plugin actions */] }; },
    onWebSocketFrame(ctx) { return { match: false }; }
  },
  actions: {
    redact: {
      params: { type: "object", required: ["names"], properties: { names: { type: "array", items: { type: "string" } } } },
      handle(ctx, params) { /* return further actions or a frame patch */ }
    }
  },
  commands: {
    explain: {
      summary: "Explain the last match",
      params: { type: "object", properties: { requestId: { type: "string" } } },
      async run(ctx, params) { return { requestId: params.requestId, note: "..." }; }
    }
  }
};

export default plugin;
```

`@chiling/plugin` is a types-only package published from this repository when implementation starts. It contains the hook context, the action union, and the JSON Schema types for config. It does not contain the daemon. Authors compile with `tsc`. The daemon loads the emitted `.js` and never runs the compiler.

A second file, `chiling.plugin.json`, is allowed and overrides the `package.json` block when both exist. Tools that should not parse `package.json` can read it. The fields are the same.

The daemon rejects a package whose entry is missing, whose `api` major it does not speak, or whose schema is not valid JSON Schema draft 2020-12. Rejection happens before the process is spawned.

## 6. API surface

Hooks are optional. Each one receives a JSON view and returns `{ match, actions }`, the same shape `ValidateOutput` already enforces. The view is a copy. Mutations in JavaScript are discarded.

| Hook | When the daemon calls it | View |
|---|---|---|
| `onRequest` | After the exchange is built, before upstream, outer plugin to inner plugin by priority | `ctx.request` as rules see it today, plus `ctx.original` |
| `onResponse` | On the unwind, after inner plugins | `ctx.request` and `ctx.response` (`status`, `headers`, `body`, `bodyTruncated`) |
| `onWebSocketFrame` | For a text or binary frame, only if this plugin's `onRequest` matched the upgrade | `ctx.frame` (`direction`, `opcode`, `payload`, `fin`) |

`match` on the plugin object is a static prefilter the daemon evaluates in Go: method, host pattern (the same matcher MITM uses), path prefix. If it does not match, the daemon does not send the hook. This keeps a redact-one-host plugin off the hot path for everything else. The hook may still return `match: false`.

Returned built-in actions are applied by the existing onion code. A plugin does not get a second, looser applier. `delay` and `throttle` keep their current caps. Body previews keep the 64 KiB capture cap. A `rewriteResponseBody` that needs more than `MaxTransformBody` (256 KiB) fails the plugin result the same way it fails a rule.

### History

With permission `history:read`, the host exposes `ctx.history.list` and `ctx.history.get`. They call the daemon, which reads `internal/history`, and return the same summary and detail shapes as `GET /v1/requests`. Results are limited (default 50, hard max 200) and bodies are the stored previews, not a stream. `history:write` is not in the first permission set. Plugins must not delete or rewrite the log.

### Actions a rule can call

`actions` registers handlers. The public type string is `plugin.<package-name>.<action>`. The package name keeps its npm scope, with `/` replaced by `.` so the type is one token (`plugin.@example.chiling-redact.redact` is ugly; store the npm name and use the escaped form only on the wire). The wire form is:

```json
{ "type": "plugin", "params": { "package": "@example/chiling-redact", "action": "redact", "names": ["authorization"] } }
```

`type: "plugin"` is one new built-in. `params.package` and `params.action` select the handler. Any other params are validated against that action's JSON Schema before the handler runs. The handler returns more actions, which Go validates again, and which may not include another `plugin` action in v1. That forbids plugin-to-plugin chains until we have a cycle rule. One level is enough for "this rule calls the redact plugin".

A rule written by an agent looks like any other rule:

```js
export default function rule(ctx) {
  if (ctx.request.host !== "api.example.com") return { match: false };
  return {
    match: true,
    actions: [
      { type: "plugin", params: { package: "@example/chiling-redact", action: "redact", names: ["authorization"] } }
    ]
  };
}
```

The rule runs in Goja. Goja does not load the plugin. The daemon, seeing a validated `plugin` action, sends `action` to that package's process.

### Commands

`commands` are functions the CLI can call. They are not hooks. They may be async. Their return value is JSON and is printed as-is. They do not implicitly change proxy state. If a command wants to add a rule, it returns a script and the agent runs `chiling rules create`, or it calls a daemon RPC that goes through the existing rule API. The first version should not let a command mutate rules inside the plugin process.

### Config

`chiling.config.schema.json` is a JSON Schema. `chiling plugin config <pkg>` prints it. `chiling plugin config set <pkg> <file>` validates the file and stores the object in the install record. The host receives the validated object as `ctx.config` on every call. Unknown keys are rejected. Secrets are not fields in this object. A secret is a name (`params.tokenFrom: "openai"`) resolved by the daemon from `~/.chiling/secrets/` (mode `0600`) and passed only to a plugin that declared `secrets:read`. The value is not written to traces, events, or history.

## 7. Plugins and rules

They are different objects.

| | Rule | Plugin |
|---|---|---|
| Who writes it | The agent, for one task, often from `rules from` | A package author |
| Where it lives | In-memory `RuleStore`, script included in `GET /v1/rules` | `~/.chiling/plugins`, enabled flag in the install record |
| Runtime | Goja, per evaluation | Node process for the daemon lifetime |
| What it can do | Return built-in actions, including one `plugin` action | Hooks, plus handlers for its own actions |
| Failure | `failureMode` on the rule | `failureMode` on the install record, default `fail-open` |

A plugin must not inject rule scripts into the store by itself. The agent can see every rule. If a plugin wants a starting script, `chiling plugin info <pkg>` includes an optional `ruleTemplate` string from the manifest, and the agent creates the rule. That keeps "what will run" auditable in `rules list`.

Ordering: enabled plugins are middleware outside rules. A plugin `onRequest` sees the original request. Rules then see any request patches the plugin's actions produced. Response hooks unwind rules first, then plugins, which matches the onion (outer plugin, inner rules). Priority is an integer on the install record, default 100, ordered like rules (priority, then package name).

Snapshots: enabling or disabling a plugin publishes a new plugin snapshot the same way rule edits publish a rule snapshot. An in-flight exchange keeps the snapshot it started with.

## 8. Install, storage, CLI

State directory: `~/.chiling/plugins/`.

```text
~/.chiling/plugins/
  installed.json          # id, version, enabled, priority, failureMode, config, permissions granted
  packages/
    @example/chiling-redact/   # npm package tree, including its own package-lock.json
  sockets/
    @example/chiling-redact.sock
```

`installed.json` is the source of truth for enablement. Deleting the package directory without updating it is an error the daemon reports, not a silent disable.

Commands, all JSON by default, `--pretty` for a short table:

```text
chiling plugin add <npm-pkg>[@version] [--enable]
chiling plugin add --path <dir> [--enable]     # local checkout, for authors and CI
chiling plugin list
chiling plugin info <pkg>
chiling plugin enable <pkg>
chiling plugin disable <pkg>
chiling plugin remove <pkg>
chiling plugin config <pkg>
chiling plugin config set <pkg> <file.json>
chiling plugin run <pkg> <command> [--param k=v]
```

`add` asks the daemon (`POST /v1/plugins`). The daemon runs `npm install --ignore-scripts --omit=dev` in a fresh directory, with `npm_config_ignore_scripts=true`. It then reads the manifest and, if `--enable` was set, spawns the host. Version is pinned in `installed.json` to the exact version npm resolved. `remove` stops the process, deletes the directory, and drops the record.

Registry and auth are the operator's npm config, passed through deliberately (`NPM_CONFIG_USERCONFIG` pointing at a file the operator named). The daemon does not embed a token and does not read the whole environment into the plugin.

`chiling plugin --help` lists the commands above. It does not list every installed plugin's commands. Those are in `plugin info`, so the top-level help stays stable and an agent can still discover them. `plugin list` rows are `{ package, version, enabled, api, permissions, hooks, actions, commands }`. `plugin info` adds the config schema and the rule template.

Control API, under `/v1/plugins`:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/plugins` | List |
| `POST` | `/v1/plugins` | Add (`{ "package", "version", "path", "enable" }`) |
| `GET` | `/v1/plugins/{id}` | Manifest, status, last error |
| `POST` | `/v1/plugins/{id}/enable` | Enable |
| `POST` | `/v1/plugins/{id}/disable` | Disable |
| `DELETE` | `/v1/plugins/{id}` | Remove |
| `PUT` | `/v1/plugins/{id}/config` | Replace config after schema check |
| `POST` | `/v1/plugins/{id}/commands/{name}` | Run a command |

Errors use the existing envelope (`error.code`, `error.message`, `error.details`).

## 9. Permissions, sandbox, versions

Declared permissions:

| Permission | Allows |
|---|---|
| `net` | `ctx.net.fetch(url)` through the daemon. No raw `fetch` in the plugin realm. Redirects are not followed unless listed. Private addresses are denied unless `net:private` is also granted. |
| `fs:data` | Read and write `~/.chiling/plugins/data/<package>/` only. |
| `history:read` | Bounded list and get, section 6. |
| `secrets:read` | Resolve named secrets. Names used are recorded on the trace. Values are not. |
| `scripts` | Allow npm lifecycle scripts at install. Default off. |
| `native` | Allow `*.node` files. Default off. v1 should refuse the package if any are present and this was not granted. |

The host preload deletes `process.binding`, `child_process`, `worker_threads`, `vm`, and the global `fetch`, and replaces `fs` with a client that only speaks to the daemon broker. This is a guardrail. A package granted `native` or `scripts` can escape it. The CLI prints those two grants in the `add` confirmation and in `plugin info` so an agent sees them in the JSON.

`chiling.api` uses semver. The daemon implements one major at a time (start at `1`). A plugin with a different major is not loaded. A newer minor than the daemon knows is loaded only if the daemon's minor is greater or equal. The types package version matches this.

Plugin package versions are exact pins. `chiling plugin add pkg@1` stores `1.2.3` as resolved. Upgrades are an explicit `plugin add pkg@1.3.0`, not a floating range reread on every daemon start.

Traces: every hook and every `plugin` action appends a `RuleTrace`-shaped record with `phase` of `request`, `response`, `frame`, or `command`, `ruleId` set to `plugin:<package>`, and the same `matched`, `actions`, `durationMs`, and `error` fields. Secrets and raw bodies beyond the preview are not copied into the trace. Events stay observational (`plugin_invoked`). They do not control forwarding.

Failure mode on the install record defaults to `fail-open`: a dead plugin is skipped and the exchange continues. `fail-closed` returns 403, same as a rule.

## 10. Agents

An agent discovers plugins without guessing flags:

1. `chiling plugin --help` for the stable verbs.
2. `chiling plugin list` for what is installed and which hooks and actions each one exposes.
3. `chiling plugin info <pkg>` for the config schema, permissions, and rule template.
4. `chiling rules create` with a `plugin` action, then `chiling verify replay <id>` as today.
5. `chiling plugin run <pkg> <command>` when the job is not a proxy mutation.

`plugin list` and `plugin info` are the machine-readable catalog. Help text for a plugin command is the `summary` plus the JSON Schema, not a free-form README. The daemon is the source of those strings so the CLI and a UI cannot drift.

## 11. Future UI

No new UI process. The trajectory already stored in `internal/session` gains activity kinds:

| Kind | When |
|---|---|
| `plugin_added` | Package installed |
| `plugin_enabled` / `plugin_disabled` | Snapshot publish |
| `plugin_removed` | Uninstall |
| `plugin_invoked` | A hook or action ran. Carries package, phase, request id, duration, error |

The request detail a UI already gets from `GET /v1/requests/{id}` includes plugin rows in `ruleTrace` because they use that struct. The UI can show them in the same trace list as Goja rules, with the package name as the label. A plugins screen is `GET /v1/plugins`: enabled state, version, permissions, last error. Enable and disable are the POST routes above. The UI does not run npm itself.

## 12. Testing for authors

Authors test against fixtures, not against a live network.

- `@chiling/plugin/testing` exports `runHook(plugin, fixture, phase)` which feeds a fixture JSON (the same shape as `~/.chiling/fixtures/<name>.json`) into the hook and returns the action list. It does not start the daemon. Authors run it under `node --test`.
- `chiling plugin add --path ./my-plugin` loads a checkout. CI in this repository will use that flag with `testdata/plugins/echo`, a package that returns a `tag`, so tests do not need a registry.
- A daemon integration test installs that path, sends one request through the proxy, and asserts the trace contains `plugin:<name>` and the tag. That is the same shape as `TestAgentLoopRecordInspectRuleVerify`.
- Schema failures (bad `api`, unknown permission, action that fails `ValidateOutput`) are unit tests on the manifest loader and do not spawn Node.
- The Node host has a table test: a fixture frame in, a JSON frame out, timeout kills the process.

Authors are told not to depend on hook call count beyond the contract in section 6. A response hook runs once per exchange on the unwind. A frame hook runs once per data frame.

## 13. Phased plan

Each phase leaves `go test ./...` green and does not change Goja rule behavior.

1. **Manifest and local load.** Parse `chiling` from `package.json`. Spawn the Node host for `--path` packages only. `onRequest` and `onResponse` may return built-in actions. Default permissions. `plugin list` and `plugin info`. No npm.
2. **npm install.** `plugin add <pkg>`, `--ignore-scripts`, exact pin, enable and disable, remove. Persist `installed.json`.
3. **Rule dispatch.** The `plugin` action. Frame hook. Failure mode. Traces and activity kinds.
4. **Broker.** `history:read`, `secrets:read`, `net`, `fs:data`, config schema. Command RPC and `plugin run`.
5. **Author kit.** Publish `@chiling/plugin` types and the testing helper. Document a sample package in the README.

Phase 1 is the first code change. This document is not that change.

## 14. Open decisions

| Decision | Recommendation | Why |
|---|---|---|
| Who runs plugin code? | Node, one long-lived process per enabled plugin. Goja stays rules-only. | npm packages do not run on Goja. A per-request process is too slow. A separate process can be killed on timeout. |
| Bun instead of Node? | Not in v1. Keep the package format runtime-neutral so it can be tried later. | Authors and `npm install` already assume Node. Bun's speed does not pay for a second compatibility target yet. |
| Shared Node process for all plugins? | No in v1. Revisit if memory shows up in `plugin list`. | Isolation and kill are worth a few processes. |
| May a plugin override `mockResponse`? | No. Plugin behavior is a `plugin` action naming `package` and `action`. | Agents must be able to tell built-ins from third-party code in a rule file. |
| May a plugin insert rules by itself? | No. It may ship a `ruleTemplate` the agent creates explicitly. | The rule list stays the auditable contract. |
| Plugin-to-plugin action calls? | Not in v1. A handler returns built-in actions only. | Avoids cycles and unbounded IPC on one exchange. |
| Where do CLI commands appear? | `chiling plugin run <pkg> <command>`. | Top-level `chiling` verbs stay stable. Discovery is `plugin info`. |
| npm lifecycle scripts | Off unless the install grants `scripts`. | Install scripts are the usual supply-chain surprise. |
| Native addons | Refuse the package unless the install grants `native`. | The preload guardrail does not contain them. |
| History writes | Not a permission in the first set. `history:read` only. | The log is evidence. Plugins should not rewrite it. |
| Secret storage | Named files under `~/.chiling/secrets`, mode `0600`, injected by the daemon, redacted in traces. | Config JSON would otherwise end up in `installed.json` and in backups. |
| API version gate | Major must match. Daemon minor must be greater than or equal to the plugin's minor. | Older daemons must not pretend to understand new hooks. |
