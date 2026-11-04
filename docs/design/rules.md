# Chiling Rule Format

Status: Proposal, not implemented
Date: 2026-11-04
Scope: A rule is match patterns plus a `pipeline` of stages. Plugins are specified in [plugins.md](./plugins.md). The JSON Schema is [schemas/rule.v1.json](../../schemas/rule.v1.json). Goja remains only for an optional `run` stage.

## Workcells

`https://github.com/lidessen/workcells` is private. This environment's GitHub credential gets 404 for it, so this document does not copy that repo's types, file names, or code. The idea taken from its description ("Pathway orchestration for functions and agents") and from the owner's note is:

- A pathway is an ordered pipeline of function stages.
- A function is registered once. A pipeline stage can call it, and an agent can call the same function as a tool.
- A stage has inputs and may publish outputs for later stages.

The concrete syntax follows GitHub Actions: `on` for the match, `uses` for the function, `with` for inputs, `if` for a condition. The list key is `pipeline`, not `steps`. Each entry is a pipeline stage.

## 1. What a rule is

A rule is one pathway the daemon compiles into one middleware in the existing onion (`ruleMiddleware` in `internal/proxy`). It is not re-parsed on each request. Publish validates it and stores a snapshot, the same way rule edits already publish a snapshot. An in-flight exchange keeps the snapshot it started with.

Request, response, and frame are phases of the exchange. Each phase holds one `pipeline`. A pipeline stage is one entry in that list: an action (`uses` or `run`), a `switch`, or a `parallel` block.

```yaml
name: mock-login
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - uses: chiling/mock-response
      with:
        status: 200
        headers:
          content-type: application/json
        body: '{"ok":true,"token":"mocked"}'
```

`chiling/...` stages are built-ins implemented in Go. They do not start Node. A plugin stage names an npm package:

```yaml
name: sign-search
on:
  request:
    method: POST
    host: api.example.com
    pathPrefix: /v1/
request:
  pipeline:
    - id: body
      uses: chiling/read-body
    - id: sign
      uses: npm:@example/request-sign@2.1.0
      with:
        secret: "${{ secrets.hmac }}"
        payload: "${{ pipeline.body.outputs.bytes }}"
      if: pipeline.body.outputs.truncated == false
```

`secrets.hmac` is a name. The daemon resolves it from `~/.chiling/secrets/` and does not write the value into the trace.

## 2. Document shape

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Stable name. The daemon still assigns `rule_<n>` as the id. |
| `on` | yes | Match. `method`, `host` (MITM-style pattern), `path`, `pathPrefix`. All listed fields must match. |
| `priority` | no | Integer, default 100. Lower runs earlier, then rule id. Same order as today. |
| `failureMode` | no | `fail-open` (default) or `fail-closed`. Does not apply to `parallel` stages. |
| `request`, `response`, `frame` | at least one | A phase. Each has a `pipeline`. |
| `on.hook` | no | A plugin-defined hook. See section 6. |

A pipeline stage is one of three shapes. `if` is allowed on all three.

### Action

| Field | Required | Meaning |
|---|---|---|
| `id` | no | Required if a later stage reads `pipeline.<id>.outputs`. |
| `uses` | one of `uses` or `run` | `chiling/<builtin>` or `npm:<package>@<version>#<action>`. The version must be the installed pin. |
| `run` | one of `uses` or `run` | Inline JavaScript. See section 8. |
| `with` | no | Inputs. String values may contain `${{ }}`. |
| `if` | no | Skip this stage when false. |
| `continueOnError` | no | Default false. A failed stage stops the rest of that pipeline. |
| `timeoutMs` | no | Default 500 (`DefaultRuleTimeout`), max 2000. The phase still shares the 2 s exchange budget. |

### Switch

| Field | Required | Meaning |
|---|---|---|
| `switch` | yes | Expression compared as a string. |
| `cases` | yes | Map of value to a nested `pipeline`. |
| `default` | no | Pipeline used when no case matches. |
| `if` | no | Skip the whole switch when false. |

Exactly one arm runs. There is no fall-through. No match and no `default` skips the switch. It is not an error.

### Parallel

| Field | Required | Meaning |
|---|---|---|
| `parallel.stages` | yes | Action stages only. Each must be effect `observe`. |
| `parallel.timeoutMs` | no | Budget for the whole block. Default 500, max 2000. |
| `if` | no | Skip the block when false. |

Built-in names:

| `uses` | Phase | Effect | Effect today |
|---|---|---|---|
| `chiling/tag` | request | mutate | `tag` |
| `chiling/set-request-header` | request | mutate | `setRequestHeader` |
| `chiling/remove-request-header` | request | mutate | `removeRequestHeader` |
| `chiling/rewrite-url` | request | mutate | `rewriteURL` |
| `chiling/mock-response` | request | mutate | `mockResponse`. Later stages in that pipeline do not run. Response pipeline of this rule still runs on the synthetic response. |
| `chiling/block` | request | mutate | `block`. Same short-circuit as mock. |
| `chiling/abort` | request | mutate | `abort`, HTTP 403. |
| `chiling/set-response-header` | response | mutate | `setResponseHeader` |
| `chiling/remove-response-header` | response | mutate | `removeResponseHeader` |
| `chiling/set-response-status` | response | mutate | `setResponseStatus` |
| `chiling/rewrite-response-body` | response | mutate | `rewriteResponseBody` |
| `chiling/delay` | response | mutate | `delay`. It changes when the client is released, so it is not observe. |
| `chiling/throttle` | response | mutate | `throttle`. Same reason. |
| `chiling/replace-frame` | frame | mutate | `replaceFrame` |
| `chiling/drop-frame` | frame | mutate | `dropFrame` |
| `chiling/read-body` | request or response | observe | No mutation. Outputs `bytes` and `truncated`. |
| `chiling/log` | any | observe | Trace row plus daemon log. No exchange change. |
| `chiling/metric` | any | observe | Counter or histogram. Name and labels in `with`. |
| `chiling/notify` | any | observe | Deliver `with` to a configured sink. Does not wait for the sink past `timeoutMs`. |
| `chiling/tee` | any | observe | Copy the snapshot (headers and body preview) to `with.url`. Not the live body. |

`mock-response` and `block` in one pipeline are a validation error.

A plugin action's `effect` is `mutate` or `observe` in its manifest. Missing `effect` means `mutate`, so it is rejected inside `parallel`.

## 3. Examples

### Mock a login

```yaml
name: mock-login
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - uses: chiling/mock-response
      with:
        status: 200
        headers:
          content-type: application/json
        body: '{"ok":true,"token":"mocked"}'
```

No plugin is activated. The compiled middleware is the current `mockResponse` action.

### Rewrite a field in the response

```yaml
name: hide-internal-role
on:
  request:
    host: api.example.com
    pathPrefix: /v1/users
response:
  pipeline:
    - uses: chiling/rewrite-response-body
      with:
        find: '"role":"internal"'
        replace: '"role":"member"'
    - uses: chiling/set-response-header
      with:
        name: X-Chiling
        value: role-rewritten
```

The body rewrite still refuses bodies over `MaxTransformBody` (256 KiB) and leaves the original stream in place.

### Branch on a decoded body

```yaml
name: decode-login-body
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - id: decoded
      uses: npm:@example/protobuf@1.4.0#decode
      with:
        message: login.LoginRequest
        encoding: protobuf
    - switch: "${{ pipeline.decoded.outputs.kind }}"
      cases:
        password:
          - uses: chiling/tag
            with:
              tags: [password-login]
        sso:
          - uses: chiling/set-request-header
            with:
              name: X-Login-Kind
              value: sso
      default:
        - uses: chiling/tag
          if: pipeline.decoded.outputs.ok == true
          with:
            tags: [other-login]
    - parallel:
        timeoutMs: 200
        stages:
          - uses: chiling/log
            with:
              message: "decoded login"
          - uses: chiling/metric
            with:
              name: login_decoded
              labels:
                ok: "${{ pipeline.decoded.outputs.ok }}"
```

`switch` runs one arm, in order, on the exchange. `parallel` does not. The log and the metric see `decoded` because that stage finished before the block. They do not see each other, and nothing after the block may read their outputs.

### Sign the request

```yaml
name: sign-upstream
on:
  request:
    method: POST
    host: api.partner.test
    pathPrefix: /v1/
request:
  pipeline:
    - id: signed
      uses: npm:@example/hmac-sign@2.1.0#sign
      with:
        secret: "${{ secrets.partner_hmac }}"
        algorithm: sha256
    - uses: chiling/set-request-header
      if: pipeline.signed.outputs.ok == true
      with:
        name: X-Signature
        value: "${{ pipeline.signed.outputs.header }}"
```

The plugin returns the header value as an output. The built-in stage applies it. The plugin does not set the header itself, so the mutation stays in the Go applier and shows up as its own trace row.

## 4. Branching

`if` may appear on an action, a `switch`, or a `parallel` block. False means that stage is skipped. The trace records `skipped`. A skipped stage publishes no outputs.

`switch` is equality dispatch, not a parallel fan-out. The expression is evaluated once and compared as a string to the case keys. The matching case's pipeline runs as a normal sequential pipeline. `default` runs when nothing matches. Cases do not fall through, and two cases never run for one value.

Nested `switch` and `parallel` inside a case are allowed. A `parallel` block's `stages` list cannot contain `switch` or another `parallel`. That keeps the schema one level deep and the observe-only check local.

`if` and `switch` overlap on purpose. `if` is a predicate. `switch` is one value, many arms. There is no `else` chain. An `else` is a `switch` with two cases, or a second stage whose `if` is the negation.

## 5. Parallelism

Stages that mutate the request or response stay strictly sequential. Header order, body replacement, and short-circuit are deterministic only if one stage finishes before the next reads the exchange.

A `parallel` block is for work that must not change the exchange: log, metric, notify, tee, or a plugin action declared `effect: observe`. The block is concurrent and non-blocking.

Semantics:

- When the block is reached, the daemon takes a snapshot of the fields those stages are allowed to read: the request or response view already captured, and outputs of stages that finished before the block. Later mutations are invisible to the block. The block does not read the live body stream. `chiling/tee` copies that snapshot, including at most the body preview, so a tee cannot force a full buffer onto the hot path.
- The pipeline continues immediately. Upstream is not held for the block. The client is not held for it.
- Stages inside the block do not see each other's outputs. A later stage in the same pipeline cannot reference `pipeline.<id>.outputs` for an id declared inside the block. Those outputs exist only on the trace, after the block finishes or times out.
- `timeoutMs` covers the block. When it fires, in-flight observe work is cancelled. The trace row is `timeout`. The exchange is unchanged.
- An error or a timeout in the block never fails the exchange and never triggers `fail-closed`. `continueOnError` is ignored inside `parallel` because the exchange has already moved on.
- `delay` and `throttle` are mutate. They change when bytes reach the client. They are rejected inside `parallel`.

If the agent needs "notify, then forward only if notify succeeded", that notify is a normal sequential stage, not a `parallel` stage. Waiting is the definition of blocking.

## 6. Expressions

`${{ }}` is not JavaScript. The grammar is small so the schema plus a walker can reject it:

- `request.<field>`, `response.<field>`, `frame.<field>` for fields already on the exchange (`method`, `host`, `path`, `url`, `status`, `body`, `truncated`, `opcode`, `direction`, `payload`).
- `pipeline.<id>.outputs.<name>` for an earlier stage in the same pipeline, not one declared inside a `parallel` block.
- `secrets.<name>` only as a whole `with` value, never concatenated. The trace stores the name, not the value.
- Comparisons in `if` and in `switch`: `==`, `!=`, `&&`, `||`, and parentheses. No calls, no arithmetic, no templates inside `if` or `switch`.

Anything else is a validation error. If a stage needs real logic, it is a plugin action or a `run` stage, not an expression.

## 7. Order and hooks

One rule is one middleware. Its pipeline is not interleaved with another rule's pipeline. Priority orders rules and always-on plugin middleware together. Lower priority runs first on the way in. Response pipelines unwind in reverse. A pathway that must run "A, then B, then A's next stage" is one rule, not two.

Inside a pipeline, stages run in source order. Outputs of stage N are visible to stage N+1, except outputs born inside `parallel`. They are not visible to other rules. Cross-rule data stays on the request (headers, URL, body, tags).

Plugin-defined hooks are named points, not a second onion:

```yaml
name: after-decode-audit
on:
  hook: example.protobuf.decoded
request:
  pipeline:
    - uses: chiling/tag
      with:
        tags: ["decoded:${{ hook.outputs.message }}"]
```

A stage emits a hook only by declaring it. The protobuf plugin's `decode` action declares `emits: [example.protobuf.decoded]`. After that stage succeeds, subscribers run, in their priority order, before the next stage of the same pipeline. A subscriber cannot emit the hook it is handling. The daemon rejects a cycle when the snapshot is published. Hook depth is one.

Phase order for one exchange:

1. Match `on.request` in Go. Do not start Node if every matched stage is `chiling/...` or `run`.
2. Activate plugins whose `uses` was matched, or whose always-on middleware matches.
3. Request phase, outer to inner. Each pathway runs its `request.pipeline`, then any hooks those stages emit. `parallel` blocks are spawned and not awaited.
4. Upstream, unless a stage short-circuited with mock, block, or abort.
5. Response phase, inner to outer: `response.pipeline` and their hooks.
6. For a WebSocket data frame, `frame.pipeline` of pathways whose request phase matched the upgrade. Ping, pong, and close are not a phase.

## 8. Authoring and validation

The YAML document is JSON after parse. [schemas/rule.v1.json](../../schemas/rule.v1.json) is the draft 2020-12 schema. It checks shape: `pipeline` is an array of action, `switch`, or `parallel`; `if` is a string; `switch` requires `cases`; `parallel.stages` is a list of actions, not nested `switch` or `parallel`; `uses` and `run` are mutually exclusive; `run.runtime` is `goja`.

`chiling rules validate <file>` runs that schema, then a walker the schema cannot express:

- Expression grammar, including the ban on reading outputs that were declared inside `parallel`.
- `uses` phase matches the phase the stage sits in (`chiling/mock-response` only under `request`, and so on).
- `parallel` stages are effect `observe`. Built-ins use the table in section 2. Plugin actions use the manifest. Missing `effect` is `mutate`.
- Every `npm:` package is installed at that version, and `with` matches the action schema. `rules validate --offline` skips this resolve.
- `mock-response` and `block` are not both in one pipeline.
- Hook `emits` graphs have no cycle.

Errors use the existing envelope. The field path looks like `request.pipeline[1].parallel.stages[0].uses`.

`chiling rules from <request-id>` emits YAML with a `pipeline`, not JavaScript. Default is one `chiling/mock-response` stage. `--action rewrite`, `header`, `block`, and `tag` select other built-ins. `--uses npm:@example/protobuf@1.4.0#decode` emits a plugin stage whose `with` keys come from that action's schema.

`chiling rules create <file.yaml>` stores the YAML. `rules get` returns it. `rules test` runs the compiled pathway against a fixture and returns the trace without forwarding. `parallel` stages run, but the test waits for them up to `timeoutMs` so the trace is complete.

Each stage is one `RuleTrace` row: `ruleId`, phase, stage `id`, `uses`, duration, error, skipped, and outputs with secrets redacted. A `parallel` stage is a row too, with `async: true`. `chiling requests show <id> --include rule-trace` is how an agent debugs a pathway.

## 9. Migration from JavaScript

Today's rule is a Goja function (`internal/rule`) returning `{ match, actions }`. That stays loadable.

A stored JS rule is treated as a pathway with one `run` stage. New `rules from` output is YAML. `rules create` accepts `.yaml`, `.yml`, and, during migration, `.js`.

Inline JS is a pipeline stage, not the document:

```yaml
name: one-off
on:
  request:
    path: /v1/special
request:
  pipeline:
    - run:
        runtime: goja
        script: |
          export default function(ctx) {
            return { match: true, actions: [{ type: "tag", params: { tags: ["special"] } }] };
          }
```

`runtime` must be `goja`. The script is the current sandbox: no `import`, no npm, 500 ms, validated actions. A `run` stage does not start the plugin host. `effect` for `run` is `mutate`, so it cannot sit inside `parallel`. A stage that only logs should call `chiling/log`.

`chiling rules migrate <file.js>` prints the wrapped YAML. It does not recover structured `uses` from arbitrary JS.

## 10. Critique

The match is data, so the daemon can skip Node and can index rules without executing them. The pipeline is data, so `rules from` and `rules validate` can be mechanical. The trace is one row per stage. Plugin and built-in stages look the same to the agent.

Branching with `if` is necessary and cheap. `switch` is the same control flow with one value and several arms, which stays validatable. It should not run arms in parallel. Exclusive and sequential is the whole point. If two arms must both do work, they are two stages with `if`, or two observe stages in `parallel`.

Limiting `parallel` to observe-only stages is the right cut. Mutating the exchange on two goroutines makes header order and body replacement races, and it makes a trace impossible to replay. The part that is easy to get wrong is "non-blocking". If later stages could read parallel outputs, the pipeline would wait, and the feature would be a join with extra syntax. Those outputs are trace-only. A stage that the rest of the pipeline depends on is sequential, even if the author thinks of it as a side effect.

`delay` and `throttle` look like side effects and are not. They change when the client sees bytes. Classifying them as mutate is deliberate.

Tee is the other trap. Copying the live body would either block the hot path or race a later rewrite. The snapshot is the preview the proxy already kept. A plugin that needs the full body says so as a sequential mutate stage and pays the transform limit.

The other risks from the pathway design still hold.

**Ordering.** Two pathways at the same priority do not interleave. `rules validate` should print the compiled order: priority, rule id, phase, pipeline index, stage id.

**Hot path.** Built-in stages must compile to the same Go calls as today's actions. Plugin stages pay worker startup on first use and a JSON round trip after that. `${{ }}` is a small interpreter in Go, not a call into Node or Goja.

**Debuggability.** A skipped `if` or an unmatched `switch` has to be a trace row. A `parallel` timeout has to name the stage. Secret values stay out of that row.

**Plugin hooks.** `emits` runs other rules in the middle of this pipeline. Depth one and cycle checks at publish time are the constraint.

**Expressions.** The grammar in section 6 is there to stop `${{ }}` becoming a language. The escape hatch is a plugin stage or `run`, not more expression functions.

**Migration.** `rules from` emitting a `pipeline`, and the README examples moving to YAML, matter more than keeping `run`.
