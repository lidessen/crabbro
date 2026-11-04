# Chiling Plugin Design

Status: Implemented, with the deferrals below. Inspection scope is [inspect.md](./inspect.md) and is not built.
Date: 2026-11-04
Revised: plugins contribute middleware, hooks, and tools. Rules are YAML pathways, specified in [rules.md](./rules.md). The host runtime is unchanged: one lazy Node host, a worker per active plugin.
Scope: JS plugins distributed as npm packages.

`NETWORK_PIPELINE_DESIGN.md` decision 7 kept plugin installation, manifests, host capabilities, and command registration out of the network work. This document is that follow-on. Agents author rules as YAML ([rules.md](./rules.md)). Goja remains only for an optional `run` pipeline stage.

Workcells (`lidessen/workcells`, read from the attached main-branch docs) is the owner's other runtime. Chiling borrows its names where they mean the same thing: `uses`, `with`, `if`, `env`, `instance`, Tools versus `uses`, explicit `middleware`, plugin instances, `config draft/validate/commit`, `preview`, per-file isolation, and a host that performs Decision calls. It does not borrow ScriptC, just-bash, durable Runs, or one OS process per plugin instance. The host here stays one Node process and a worker per active plugin.

## 1. Decision summary

1. Plugins are npm packages. The daemon does not execute them inside Goja.
2. A plugin contributes three things: middleware in the proxy onion, hooks other pathways can attach to, and tools an agent calls from the CLI.
3. Node is the plugin runtime. The daemon supervises **one** host process, not one process per plugin. The host starts lazily and exits when nothing is active.
4. Each activated plugin runs in its own `worker_threads` worker. A timeout terminates that worker. Native code, or `isolation: process`, gets a child process. WASM is not the npm runtime.
5. A pipeline stage is `uses: chiling.<capability>`, `uses: <plugin>.<capability>`, or a fallback `run` script. Go checks `with` against the action schema. Plugin code never receives `http.Request` or a socket.
6. Built-in names under `chiling.*` are reserved. A plugin cannot replace `chiling.mock-response`.
7. Default permissions are none. `worker_threads` is not a security boundary. See section 10.
8. Tools are `chiling plugin invoke <plugin> <instance> <tool>`. Tools and `uses` are separate catalogs. They may share one function. They do not imply each other, and they do not add top-level `chiling` verbs.
9. Install, enable, and traces go through the daemon. The CLI is a client. Output is JSON. `--json` is accepted and is the default. `--pretty` is the human form.

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

A hot-path rule must not grow a Node dependency. A distributed plugin must not be squeezed into a runtime that cannot load one.

## 3. Goals and non-goals

### Goals

- An operator or agent can install a plugin from npm and enable it without editing the daemon.
- Idle plugins cost nothing in Node. Enabled-but-unmatched plugins do not keep a process or a worker alive.
- Plugin authors write TypeScript against a types-only package and ship compiled JavaScript.
- Hooks line up with the stages the proxy already has: request, response, WebSocket frame.
- Anything a plugin returns is checked by the same validator as a rule, then applied by Go.
- A runaway or crashed plugin does not take down `chilingd`, and does not take down unrelated plugins.
- Agents can list plugins, read their manifests, and call their commands from JSON.
- A UI can show install, enable, and per-exchange plugin traces without a second control plane.

### Non-goals for the first implementation

- Running anonymous untrusted code as if it were a browser origin. Installing a package is an operator decision. The permission layer stops accidents and surprise install scripts. It does not contain a hostile native addon. See section 10.
- Replacing the YAML pathway with a plugin. One-off rewrites are built-in pipeline stages. `run` is the escape hatch, not the format.
- Loading plugins from the network on each request.
- A plugin store inside chiling. npm is the index.
- Plugin-owned MITM policy. A plugin match must not decrypt a host. That stays `NetworkPolicy`.
- Editing history or deleting captures in the first permission set.
- Compiling npm packages to WASM so they can run inside the Go process.

## 4. What other systems actually do

These are the arrangements that matter for a local proxy whose plugins are npm packages. The column "isolation" is what a faulty or hostile plugin can do to its neighbors, not the marketing claim.

| System | Where code runs | Idle cost | How a bad plugin is stopped | Isolation that is real | What it gives up |
|---|---|---|---|---|---|
| VS Code extension host | One Node process (sometimes a few: local, remote, web). Extensions are modules in that process. Lazy activation on events. JSON-RPC to the workbench. | An enabled extension that has not activated costs a manifest. The host process exists once extensions are needed. | Host crash restarts the **whole host**. Every extension in it reloads. A stuck extension can freeze all of them. | API surface and a separate process from the UI. Extensions in one host share a heap and can `require` Node. | Per-extension kill. |
| JetBrains | In-process JVM. Classloader per plugin. Declarative extension points in `plugin.xml`. | Loaded plugins sit in the IDE process. | A bad plugin can crash or deadlock the IDE. Dynamic unload exists and is partial. | Classloader, not an OS process. | Crash isolation. |
| Chrome extensions, MV3 | A service worker per extension, event-driven, killed when idle. Separate from the page. `declarativeNetRequest` for network rules. | Idle extension has no running worker. | The worker is terminated. Other extensions stay up. | Process-per-extension in Chromium, plus a permission manifest. No arbitrary Node. | Persistent background state. Blocking `webRequest` is mostly gone. |
| Figma plugins | A sandboxed JS realm per plugin. `postMessage` RPC to the document. The document API is the capability set. | A closed plugin is not running. | The realm can be torn down without closing the file. | The plugin never holds the document objects. Network exists in the iframe unless the product restricts it. | npm, Node, and a general filesystem. |
| Obsidian | JS evaluated in the app process. Full Node and vault access. | Loaded plugins are part of the app. | A throw or tight loop can break the app. | Community trust. Almost none technically. | Safety. |
| Deno | One process. Permissions are capabilities (`--allow-net`, `--allow-read`). A worker can be created with a **subset** of those permissions. | The process you started. | Kill the worker or the process. | The permission check is in the runtime, for Deno APIs. It is not a VM boundary against native code. | npm semantics are an emulation layer, not the reason Deno exists. |
| Zed extensions | WASM components (Wasmtime, WIT) in the editor process. | An unused extension is bytes on disk. Instantiation is cheap relative to Node. | A trap fails that instance. The editor stays up. Memory can be capped per instance. | Strong for code that only uses the host ABI. | No npm. Authors compile to WASM. |
| Envoy proxy-wasm | WASM VM inside the proxy process. SDK in Rust, Go, AssemblyScript, C++. One ABI for request and response headers and buffered bodies. | Configured filters occupy a VM. | A trap can fail the stream (fail-open or fail-closed). Some runtime bugs have still crashed Envoy. | Good for pure WASM. Not perfect against runtime bugs. | Not JavaScript npm. Body streaming and host calls are an explicit ABI. |
| mitmproxy addons | Python in the mitmproxy process. Hooks: `request`, `response`, `websocket_message`. | Imported addons live in the process. | An exception is logged. A blocking call blocks the proxy. | None. This is the "local user script" model. | Isolation. It is the closest product analogue to chiling rules, with a much larger language surface. |
| Whistle | npm plugins loaded into the Whistle Node process. | In-process. | A bad plugin can crash Whistle. | None beyond Node modules. | Isolation. |
| Charles, Proxyman | In-app tools and JavaScript **scripts** for rewrite, map-local, and breakpoints. Not a general npm host. | Scripts are part of the app session. | A script error fails that script. | The script API is smaller than Node. | Distribution as packages. |
| Caddy | Go modules **compiled into** the binary (`xcaddy`). | No extra process. | A bad plugin is a bad build. | Trust is at build time. | `npm install` without a rebuild. |
| Vite, Rollup | JS hooks in the bundler process (`resolveId`, `load`, `transform`). | In-process. | A plugin can hang the build. | None. Authors are trusted. | A security story. The hook *shape* (pure functions, narrow context) is still the right API idea. |
| Grafana backend plugins | Hashicorp `go-plugin`: one OS process per plugin, gRPC over a local socket, version handshake, supervisor restarts the plugin. | One process per installed backend plugin, even when idle. | RPC deadline plus process kill. Grafana stays up. Other plugins stay up. | Address space and kill are real. The plugin runs as the same OS user, so it can still read the disk. | Memory: a full process per plugin, which is the cost this revision is trying not to pay for ordinary JS. |

What is worth copying, and what is not:

- **Copy from VS Code:** one host, activation events, lazy load, a versioned RPC, restart the host only when the host itself is dead.
- **Copy from Chrome MV3:** an idle plugin is not a running worker. Wake it on an event. Do not keep N runtimes warm.
- **Copy from Grafana `go-plugin`:** the daemon supervises the host (handshake, heartbeat, restart). Use a **separate process** only when the plugin can crash native code.
- **Copy from Envoy and Zed:** the plugin returns a patch through a narrow ABI. A failure fails that stream, not the proxy. Do not copy WASM as the v1 implementation.
- **Copy from Vite and mitmproxy:** hooks are small and synchronous from the proxy's point of view. Chiling already has this for rules.
- **Do not copy Caddy's compile-in model.** It rejects the npm requirement.
- **Do not copy Obsidian or Whistle's in-process trust** for the Go daemon. A plugin must not share an address space with `chilingd`. The proxy has to keep forwarding if Node dies.

## 5. Runtime

### Recommendation

Three tiers. Almost every plugin is tier 1.

| Tier | When | What runs it |
|---|---|---|
| Rules | YAML `pipeline`, compiled in Go. `run` is the optional Goja fallback. | The daemon. Goja only when a stage is `run`. |
| Plugins, default | npm package, pure JS, no `native` grant. | A worker inside the single Node host. |
| Plugins, opted in | Manifest sets `"isolation": "process"`, or the install granted `native`. | A child process of the host, supervised like a Grafana plugin. One plugin, one process. |

The host is a small program shipped with chiling (`cmd/chiling-plugin-host`). `chilingd` speaks to it over one Unix socket, mode `0600`. The daemon does not start the host at boot. It starts the host on the first activation event. If every worker has been idle and the host has been idle for a grace period, the daemon stops the host. Steady state with no matching traffic is zero Node processes.

Workers:

- Created on that plugin's first activation, not at enable time.
- Destroyed with `worker.terminate()` after an idle timeout (proposal: 30 s with no hook and no command).
- Given V8 `resourceLimits` (proposal: 64 MB old-generation heap, overridable in the install record).
- Do not share a JS heap. They do not share plugin config objects. The host keeps the config and passes a copy in each call.

This is cheaper than one Node process per plugin once two or more plugins are active, and it is as cheap when they are idle, because neither design should keep a runtime for an idle plugin. The earlier per-process recommendation paid a full Node startup and a full RSS **per active plugin**, plus one socket and one supervisor each. The host pays one Node startup for the whole set, and a worker isolate per plugin that is actually in a hook. A worker isolate is still real memory (its own V8 heap). It is not free. It is smaller and faster to start than a process, and one heartbeat covers all of them.

Bun is not the v1 host. The package format does not name the runtime, so a later host can be swapped. Goja stays rules-only. Teaching Goja to load npm would fail on `import`, top-level await, and dependencies.

WASM stays a documented non-goal for v1. proxy-wasm and Zed are what to copy if chiling ever wants a filter that must not be able to call Node at all. That filter would be a second kind of package, not an npm compatibility layer. Componentizing npm to WASM is not a migration path authors will accept.

### Activation

The manifest lists activation events. The daemon evaluates them in Go, before any Node code runs.

| Event | Fires when |
|---|---|
| `onRequest` | A request passes the plugin's static `match` (method, host pattern, path prefix). This is the default if the manifest omits `activation`. |
| `onCommand` | `chiling plugin invoke`, `plugin tools`, or `plugin inspect` names this package. |
| `onStartup` | The daemon is up and the plugin is enabled. Opt-in. `plugin inspect` warns. Idle cost stops being zero. |

`onRequest` uses the same host matcher as MITM (`internal/mitm` patterns). A plugin for `api.example.com` does not wake on other hosts. Enablement without a matching request does not start Node.

### Timeouts and runaway plugins

The daemon owns the deadline, same idea as Goja's interrupt, different mechanism.

1. The daemon's RPC wait is the hook budget. Proposal: 500 ms by default, the same `DefaultRuleTimeout`, overridable per plugin up to the exchange's `DefaultRuleBudget` (2 s). A frame hook uses the same budget per frame.
2. On deadline, the daemon tells the host to terminate **that worker** and applies the plugin's failure mode (`fail-open` skips it, `fail-closed` returns 403). Other workers and the Go proxy keep going.
3. `worker.terminate()` is asynchronous and does not stop a thread stuck inside native code. If the worker has not exited within 1 s, the host marks the plugin poisoned and refuses new calls until the worker is actually gone. The in-flight exchange has already moved on at step 2.
4. The host heartbeats every 2 s. Three missed beats and the daemon kills the **host** process and starts a new one on the next activation. That is the VS Code restart, used only when the supervisor itself is dead, not when one plugin is slow.
5. A plugin that exceeds its heap limit crashes its worker (V8 abort of that isolate). The host reports it. The daemon does not restart the host.
6. In-flight exchanges keep the plugin snapshot they started with. A restart does not retarget them.

### Memory

- Per worker: `resourceLimits.maxOldGenerationSizeMb` (default 64). This caps the V8 heap. It does not cap native allocations, buffers held by the host, or memory the plugin causes the daemon to spend (history reads are already limited).
- Per host: the daemon samples RSS. Proposal: soft cap 512 MB for the whole host. Over the cap, the daemon restarts the host and records `plugin_host_restarted`. This is a backstop, not a fair scheduler.
- A plugin that declares `isolation: process` has its own RSS. The host cap does not include it. The daemon applies a separate cap (proposal: 256 MB) and kills that process if it exceeds it.

### RPC

One socket, `~/.chiling/plugins/host.sock`, mode `0600`. Frames are length-prefixed JSON (`uint32` big-endian length, then one object). gRPC would match Grafana and cost a code generator for a protocol that only our host and our daemon speak. JSON matches the CLI and the rule action schema.

Daemon to host:

| `type` | Fields | Meaning |
|---|---|---|
| `hello` | `api`, `token` | Handshake. Host replies with its protocol version. Mismatch: daemon refuses the host. |
| `activate` | `package`, `entry`, `config`, `permissions` | Start the worker if it is not running. |
| `hook` | `id`, `package`, `hook`, `exchangeId`, `payload` | `onRequest`, `onResponse`, or `onWebSocketFrame`. |
| `uses` | `id`, `instance`, `capability`, `with`, `payload` | A pipeline `uses` call. |
| `tool` | `id`, `instance`, `tool`, `arguments` | `plugin invoke`. |
| `deactivate` | `package` | `worker.terminate()` after the current call. |
| `shutdown` | | Exit the host. |

Host to daemon:

| `type` | Fields | Meaning |
|---|---|---|
| `result` | `id`, `match`, `actions`, `error` | Reply to `hook`, `action`, or `command`. |
| `broker` | `id`, `package`, `op`, `payload` | `history.list`, `history.get`, `net.fetch`, `fs.read`, `fs.write`, `secrets.get`. The daemon checks the permission and the limits, then replies. |
| `log` | `package`, `level`, `message` | stderr for the operator. Not a trace. |
| `heartbeat` | `rssBytes` | Liveness and the memory sample. |

`id` correlates replies. Hooks on different exchanges run concurrently. The host queues per worker so one plugin's hooks are ordered and two plugins are not blocked on each other. The daemon does not wait for plugin A before starting plugin B's hook.

The host is the only Node process that loads plugin code. Workers cannot open the daemon socket. Broker calls go through the host, which stamps the package name. A worker that guesses another package's name still fails the permission check in the daemon, and the host drops broker frames whose package does not match the worker that sent them.

## 6. Package shape

A plugin is a normal npm package. The daemon reads the `chiling` block as data. It does not execute `package.json`.

```json
{
  "name": "@example/chiling-redact",
  "version": "1.2.0",
  "type": "module",
  "exports": { ".": "./dist/index.js" },
  "chiling": {
    "api": "1.0.0",
    "entry": "./dist/index.js",
    "activation": ["onRequest"],
    "isolation": "worker",
    "permissions": ["history:read"],
    "configSchema": "./chiling.config.schema.json"
  }
}
```

| Field | Required | Meaning |
|---|---|---|
| `chiling.api` | yes | Semver of the plugin API. The daemon loads it only when the major matches and the daemon's minor is at least the plugin's minor. |
| `chiling.entry` | yes | JavaScript file, relative to the package root, no `..`. |
| `chiling.activation` | no | `onRequest` (default), `onCommand`, `onStartup`. |
| `chiling.isolation` | no | `worker` (default) or `process`. |
| `chiling.permissions` | no | Declared capabilities. Default is empty. |
| `chiling.configSchema` | no | JSON Schema for the plugin's config object. |

The entry module default-exports one object:

```ts
import type { Plugin } from "@chiling/plugin";

const plugin: Plugin = {
  match: { hosts: ["*.example.com"], methods: ["POST"] },
  hooks: {
    onRequest(ctx) { return { match: false }; },
    onResponse(ctx) { return { match: true, actions: [] }; },
    onWebSocketFrame(ctx) { return { match: false }; }
  },
  actions: {
    redact: {
      params: { type: "object", required: ["names"], properties: { names: { type: "array", items: { type: "string" } } } },
      effect: "mutate",
      handle(ctx, params) { return { match: true, actions: [] }; }
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

`@chiling/plugin` is a types-only package published from this repository when implementation starts. It contains the hook context, the action union, and the config types. It does not contain the daemon. Authors compile with `tsc`. The daemon loads the emitted `.js` and never runs the compiler.

`chiling.plugin.json` is allowed and overrides the `package.json` block when both exist. The daemon rejects a package whose entry is missing, whose `api` it does not speak, or whose schema is not valid JSON Schema draft 2020-12. Rejection happens before a worker is started.

## 7. API surface

Hooks are optional. Each receives a JSON view and returns `{ match, actions }`, the same shape `ValidateOutput` already enforces. The view is a copy.

| Hook | When | View |
|---|---|---|
| `onRequest` | After the exchange is built, before upstream. Outer plugin to inner plugin by priority. | `ctx.request` as rules see it, plus `ctx.original` |
| `onResponse` | On the unwind, after inner plugins. | `ctx.request` and `ctx.response` (`status`, `headers`, `body`, `bodyTruncated`) |
| `onWebSocketFrame` | A text or binary frame, and only if this plugin's `onRequest` matched the upgrade. | `ctx.frame` (`direction`, `opcode`, `payload`, `fin`) |

`match` on the plugin object is the static prefilter the daemon evaluates in Go. If it does not match, the daemon does not send the hook and does not activate the plugin. The hook may still return `match: false`.

Returned built-in actions are applied by the existing onion. `delay` and `throttle` keep their current caps. Body previews keep the 64 KiB capture cap. A `rewriteResponseBody` that needs more than `MaxTransformBody` (256 KiB) fails that plugin result the same way it fails a rule.

### History

With `history:read`, `ctx.history.list` and `ctx.history.get` call the daemon over the broker and return the same shapes as `GET /v1/requests`. Default limit 50, hard max 200. Bodies are stored previews. `history:write` is not in the first permission set.

### Actions a rule can call

Public names follow workcells. A builtin is `chiling.<capability>`. A plugin capability is `<plugin>.<capability>`, from the `path` segments on a `uses` entry (`["headers"]` on plugin `redact` is `redact.headers`). A rule does not pass `package` and `action` as a fake action type. Each action declares `effect: "mutate"` or `effect: "observe"`. Only `observe` is legal inside `parallel`. See [rules.md](./rules.md). The handler returns built-in mutations or outputs. In v1 those mutations are built-ins, not another plugin `uses`.

The stage is:

```yaml
- id: redact
  uses: redact.headers
  instance: local
  with:
    names: ["authorization"]
```

`instance` defaults to `local`. The install record pins the package digest. The handler returns built-in mutations or outputs. In v1 a handler's returned mutations are built-ins, not another plugin `uses`. That forbids plugin-to-plugin chains until there is a cycle rule.

### Tools, uses, and middleware

These are three catalogs, as in workcells. They may share one function. They do not imply each other.

| Catalog | Invoked by | Creates a proxied exchange? |
|---|---|---|
| `tools` | `chiling plugin invoke <plugin> <instance> <tool>` | No |
| `uses` | A pipeline stage `uses: <plugin>.<capability>` | Only as part of the request already in flight |
| `middleware` | A stage's `middleware:` list, or `context.assemble`-style explicit selection | No, until a stage selects it |

Registering a plugin does not insert it into the onion. The host checks the named instance's permissions on every reference. A stage that selects middleware:

```yaml
- id: policy
  uses: chiling.pass
  middleware:
    - { plugin: redact, instance: local, name: strip-auth }
```

Always-on middleware from an earlier draft is dropped. Install must not change traffic the author did not name.

### Instances and credentials

`chiling plugin instance add/configure/list/enable/disable/remove`. One package, many named instances (`local`, `primary`), each with its own non-secret config and credential refs. The host injects credentials at call time. They are not accepted in `vars`, `env`, `with`, or instance config files. `chiling plugin invoke` calls a Tool and does not create a rule run.

Plain `plugin list` and `--help` read manifests and do not start workers. Tool discovery (`plugin tools`, `plugin inspect`) does run trusted plugin code. Instances activate on demand and are reused. A call does not spawn a process.

### Install

`chiling plugin install ./pkg` requires a complete `package-lock.json`. The installer runs `npm ci --ignore-scripts` and does not run lifecycle scripts or the repo's build script. Unlike workcells, it does not compile with ScriptC. The result is the JS entry the Node host loads into a worker. Help text and manifest listing still do not start that worker.

### Decision

Workcells' Decision plugin is the model for chiling's existing Jev and OpenAI calls. The plugin exposes `decision.ask` as a Tool and as a `uses`. The host performs HTTP, holds the credential ref, and returns a validated `choice` or `noul`. The pipeline cannot set the endpoint, headers, or API key. A sink file (mode `0600`, not a symlink) binds instance, package digest, principal, endpoint, and model. Chiling keeps both Jev and an OpenAI-compatible endpoint. It does not adopt workcells' lock to `api.commandcode.ai` or Keyref. Diagnostics (`decisionDiagnostic`) carry a fixed category, `requestMayHaveBeenSent`, and optional status and duration. They never carry the credential, request headers, or upstream body. One System One or chat call per plugin call. A timeout does not prove the provider did not charge, and the host does not retry automatically.

### Commands

Tools are not hooks. Their return value is JSON. They do not change proxy state inside the worker. A tool that wants a rule returns YAML, and the agent runs `chiling rules commit`.

### Config

Non-secret config belongs to a named instance: `chiling plugin instance configure <plugin> <instance> --config <file>`. The host validates it against the package schema and passes a copy as `ctx.config` on each call. Credentials are refs in that file, resolved by the host at call time from `~/.chiling/secrets/` (mode `0600`). They are not fields the rule can set, and they are not written to traces, events, or history. A plugin reads a resolved value only when the instance was granted `secrets:read`. The name is recorded on the trace. The value is not.

## 8. Plugins and rules

| | Rule | Plugin |
|---|---|---|
| Who writes it | The agent, often from `rules from` | A package author |
| Where it lives | In-memory `RuleStore` | `~/.chiling/plugins` |
| Runtime | Goja, per evaluation, inside `chilingd` | Worker in the Node host, or a private process if it opted into one |
| What it can do | Built-in `uses`, plugin `uses`, and a fallback `run` | Tools, `uses`, and middleware a stage names |
| Failure | `failureMode` on the rule | `failureMode` on the install record, default `fail-open` |

A plugin must not insert rules by itself. `plugin inspect` may include a `ruleTemplate` string. The agent creates the rule. The rule list stays the auditable contract.

Ordering: plugins are middleware outside rules. Plugin `onRequest` sees the original request. Rules then see patches from plugin actions. Response hooks unwind rules first, then plugins. Priority is an integer on the install record, default 100, then package name.

Enabling or disabling a plugin publishes a new snapshot. An in-flight exchange keeps the snapshot it started with. Deactivation terminates the worker after the current call. It does not stop the host if other plugins are active.

## 9. Install, storage, CLI

```text
~/.chiling/plugins/
  installed.json              # package, resolved version, enabled, priority, failureMode, isolation, config, granted permissions
  host.sock                   # the one RPC socket, mode 0600
  packages/
    @example/chiling-redact/  # npm tree and its package-lock.json
  data/
    @example/chiling-redact/  # only if fs:data was granted
```

There is not a socket per plugin. `installed.json` is the source of truth for enablement.

Commands, JSON by default, `--pretty` for a short table:

```text
chiling plugin install <path> [--enable]
chiling plugin upgrade <id>
chiling plugin uninstall <id>
chiling plugin list
chiling plugin inspect <id> --json
chiling plugin tools <id>
chiling plugin instance add <plugin> <instance> --config <file>
chiling plugin instance configure <plugin> <instance> --config <file>
chiling plugin instance list <plugin>
chiling plugin instance enable <plugin> <instance>
chiling plugin instance disable <plugin> <instance>
chiling plugin instance remove <plugin> <instance>
chiling plugin invoke <plugin> <instance> <tool> --arguments <json> --json
chiling config status
chiling config draft <file>
chiling config validate <file> --json
chiling config commit <file>
chiling config discard <file>
chiling plugin host status
```

`install` requires `package-lock.json` and runs `npm ci --ignore-scripts`. It does not run lifecycle scripts. It does not start a worker. `inspect` and `tools` do run trusted plugin code for discovery. `list` and `--help` do not. `invoke` calls a Tool and does not open a proxy exchange. `uninstall` stops that plugin's worker and drops the package. If it was the last worker, the host exits.

`plugin list` rows include `{ package, version, enabled, state, api, isolation, permissions, hooks, actions, commands }`. `state` is `idle`, `running`, or `poisoned`. `plugin host status` is `{ running, pid, rssBytes, workers }`.

Registry and auth are the operator's npm config (`NPM_CONFIG_USERCONFIG`). The daemon does not copy that environment into the worker.

Control API:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/plugins` | List |
| `POST` | `/v1/plugins` | Add (`{ "package", "version", "path", "enable" }`) |
| `GET` | `/v1/plugins/{id}` | Manifest, state, last error |
| `POST` | `/v1/plugins/{id}/enable` | Enable |
| `POST` | `/v1/plugins/{id}/disable` | Disable and terminate the worker |
| `DELETE` | `/v1/plugins/{id}` | Remove |
| `PUT` | `/v1/plugins/{id}/config` | Replace config after schema check |
| `POST` | `/v1/plugins/{id}/instances/{instance}/tools/{name}` | Invoke a Tool (activates `onCommand`) |
| `GET` | `/v1/plugins/host` | Host process status |

Errors use the existing envelope (`error.code`, `error.message`, `error.details`).

## 10. Permissions, and what a worker does not isolate

Declared permissions:

| Permission | Allows |
|---|---|
| `net` | `ctx.net.fetch(url)` through the daemon. Redirects are not followed unless listed. Private addresses need `net:private` as well. |
| `fs:data` | Read and write `~/.chiling/plugins/data/<package>/` only, through the broker. |
| `history:read` | Bounded list and get. |
| `secrets:read` | Resolve named secrets. The name is recorded on the trace. The value is not. |
| `scripts` | Not a grant. The installer never runs npm lifecycle scripts or the package build script, matching workcells. |
| `native` | `*.node` files. Forces `isolation: process`. Default off. |

`worker_threads` gives each plugin its own V8 isolate, its own JS heap, and a `terminate()` that does not kill the host. A tight loop in one worker does not freeze the host's event loop or another worker. `resourceLimits` can abort that isolate when the heap grows. Those are the operational properties this design depends on.

`worker_threads` does **not** do the following. Node's own documentation treats threads as a concurrency tool, not a sandbox.

- It does not hide Node built-ins. A worker can `import('node:fs')` and `import('node:net')` unless the host's loader refuses those specifiers. The permission model in Node (`--permission`) is process-wide, so it cannot grant `net` to one worker and deny it to another.
- It does not stop a plugin from reading another plugin's files on disk. The process uid is the operator. `~/.chiling/plugins/packages` is readable. The broker is what stops the *API* from doing it. A worker that is allowed to evaluate arbitrary JS and can reach `fs` does not need the API.
- It does not cap native memory, and `terminate()` does not unwind a stuck native call.
- A bug in a native addon loaded in any worker can corrupt the host. That is why `native` forces a separate process.
- SharedArrayBuffer is opt-in and should not be enabled. Do not pass `transferList` of host secrets.

So the enforcement plan is:

1. The worker is started with a custom module loader that allows only the plugin's own package and `@chiling/plugin` (types are erased; the runtime shim is the broker client). `node:fs`, `node:net`, `node:child_process`, `node:vm`, `node:worker_threads` are blocked.
2. `fetch` is not on the global object. `ctx.net.fetch` is a function the shim sends as a broker frame.
3. The daemon checks the permission again. The host check is not the only check.
4. This stops accidents and most unmodified packages that expect full Node. It does not stop a package written to escape a loader (prototype tricks against a buggy shim, a bug in Node). Treat it the way VS Code treats the extension host: trusted-enough code the operator installed, in a process that is not the proxy.
5. `isolation: process` is the escape hatch when that is not enough. That process is still the same OS user. It is not a container. It does mean a crash and a `kill` stay inside that plugin.

`chiling.api` is semver. One major at a time, starting at `1`. Exact version pins in `installed.json`. Upgrades are an explicit `plugin upgrade`, not a floating range on daemon start.

Traces use the existing `RuleTrace` shape with `phase` of `request`, `response`, `frame`, or `command`, and `ruleId` of `plugin:<package>`. Events (`plugin_invoked`) do not control forwarding.

## 11. Agents

1. `chiling plugin --help` for the stable verbs.
2. `chiling plugin list` for what is installed, whether it is idle or running, and which hooks, tools, and `uses` it exposes. This does not start a worker.
3. `chiling plugin inspect <id> --json` for the config schema, permissions, isolation, activation events, and rule template.
4. `chiling rules commit` with a `uses` stage, then `chiling verify replay <id>`.
5. `chiling plugin invoke <plugin> <instance> <tool>` for work that is not a proxy mutation.
6. `chiling plugin host status` to see whether Node is running at all.

`plugin list` does not start a worker. `plugin inspect` and `plugin tools` do, because discovery runs trusted plugin code. Help text is the `summary` plus the JSON Schema, served by the daemon so the CLI and a UI cannot drift.

## 12. Future UI

Activity kinds on the trajectory `internal/session` already stores:

| Kind | When |
|---|---|
| `plugin_added` | Package installed |
| `plugin_enabled` / `plugin_disabled` | Snapshot publish |
| `plugin_removed` | Uninstall |
| `plugin_activated` / `plugin_deactivated` | Worker started or idle-terminated |
| `plugin_invoked` | A hook or action ran. Package, phase, request id, duration, error |
| `plugin_host_restarted` | The host process was killed and will be started again on the next activation |

`GET /v1/requests/{id}` already returns `ruleTrace`. Plugin rows use that struct. A plugins screen is `GET /v1/plugins` plus `GET /v1/plugins/host`. The UI does not run npm and does not embed Node.

## 13. Testing for authors

- `@chiling/plugin/testing` exports `runHook(plugin, fixture, phase)`. It feeds fixture JSON (the same shape as `~/.chiling/fixtures/<name>.json`) into the hook in-process. It does not start the daemon. Authors run it under `node --test`.
- `chiling plugin install ./my-plugin` loads a checkout that already has `package-lock.json`. CI uses that with `testdata/plugins/echo`, a package that returns a `tag`, so tests do not need a registry.
- A daemon test installs that path, sends one request through the proxy, and asserts the trace contains `plugin:<name>` and the tag. Same shape as `TestAgentLoopRecordInspectRuleVerify`.
- A second test installs two path plugins, makes the first one loop, and asserts the second still returns its tag and the daemon is still up. That is the regression this runtime exists to pass.
- Manifest failures do not spawn Node.
- The host has a table test for the frame protocol. A timeout test asserts `worker.terminate()` and a `fail-open` trace, and asserts the host pid did not change.

A response hook runs once per exchange on the unwind. A frame hook runs once per data frame.

## 14. Phased plan

Each phase leaves `go test ./...` green and does not change Goja rule behavior.

1. **Host and local load.** One host process, one worker per activated path plugin, `onRequest` and `onResponse` returning built-in actions. Activation on match. Idle workers exit. `plugin list`, `plugin inspect`, `plugin host status`. No npm.
2. **npm install.** `plugin install` with `npm ci --ignore-scripts`, exact pin, instances, upgrade, uninstall.
3. **Rule dispatch and faults.** Pipeline `uses`. Frame hook. Worker timeout and poison. Host heartbeat and restart. Traces and activity kinds. The two-plugin timeout test from section 13.
4. **Broker.** `history:read`, `secrets:read`, `net`, `fs:data`, instance config, `plugin invoke`. `native` and `isolation: process` as a child process.
5. **Author kit.** Publish `@chiling/plugin` and the testing helper.

The host, install, instances, `uses`, and the decision sink are in the tree. Still deferred: plugin `emits` chains, `chiling.pass`, plugin-to-plugin `uses`, `history:read` until a store is attached on the broker, and the traffic-inspection scope in [inspect.md](./inspect.md). `net.fetch` stays denied until a caller has `net`. `onStartup` does not keep the host warm.

## 15. Decisions

| Decision | Recommendation | Trade-off against the previous per-process design |
|---|---|---|
| Who runs plugin code? | Goja for rules. One Node host for plugins. A worker per activated plugin. | Per-process isolates crashes and `kill` better, and Node's `--permission` flag could apply per plugin because it is process-wide. It costs a Node RSS and a startup per **active** plugin, and N supervisors. Idle cost can be zero either way if activation is lazy. The waste the earlier draft actually had was keeping a process per **enabled** plugin. |
| When does a plugin get its own process? | Only if it sets `isolation: process` or is granted `native`. | Those plugins pay the cost the default path avoids. They can crash without taking the host down. |
| WASM? | Not for npm plugins. Revisit only for a separate untrusted ABI. | WASM (Zed, Envoy) isolates better than workers and can run inside `chilingd`. It does not run npm. |
| Bun? | Not in v1. Package format stays runtime-neutral. | Bun starts faster. Authors and `npm install` already assume Node. |
| Shared isolate for all plugins, VS Code style? | No. Workers are the default, even though VS Code shares one isolate. | A shared isolate is less memory and simpler RPC. One tight loop or one heap blow-up stalls every plugin, which is the failure VS Code accepts and a proxy should not. |
| Idle plugins | No worker and, after a grace period, no host. Activation events in the manifest. `onStartup` is opt-in. | Matches Chrome MV3 and VS Code activation. The first matched request pays host startup once. |
| Timeouts | Daemon deadline, then `worker.terminate()`. Restart the host only if the heartbeat dies. | Per-process used kill as the only tool. Terminate is weaker against native code, which is why native code is not allowed in a worker. |
| Memory | 64 MB V8 heap per worker, 512 MB RSS backstop on the host. | `resourceLimits` does not cover native RAM. The host cap is a blunt restart. |
| Permissions | Broker in the daemon, loader in the worker that does not expose Node I/O. Document that this is not a sandbox. | A dedicated process does not sandbox the disk either, unless the OS user changes. It does limit blast radius of a crash. |
| May a plugin override `mockResponse`? | No. `chiling.*` is reserved. A plugin `uses` path cannot shadow it. | The earlier `type: plugin` plus `package` and `action` shape is dropped. The stage is `uses`. |
| May a plugin insert rules by itself? | No. It may ship a `ruleTemplate` the agent creates. | Unchanged. |
| Plugin-to-plugin calls? | Not in v1. Handlers return built-in actions only. | Unchanged. Avoids cycles on one exchange. |
| Where do CLI commands appear? | `chiling plugin invoke <plugin> <instance> <tool>`. | Tools are a catalog, not a top-level verb and not the same entry as `uses`. |
| npm lifecycle scripts | Never run. No `scripts` grant turns them on. | Workcells' installer rule. A hostile `postinstall` must not run because the operator installed a package. |
| Native addons | Refuse unless granted, and then `isolation: process`. | Stronger than the previous draft, which left native code inside the same kind of process as ordinary plugins. |
| History writes | Not in the first permission set. | Unchanged. |
| Secrets | Named files under `~/.chiling/secrets`, mode `0600`, redacted in traces. | Unchanged. |
| API version gate | Major must match. Daemon minor must be at least the plugin's minor. | Unchanged. |
