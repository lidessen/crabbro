# Crabbro Rule Format

Status: Implemented, with the corrections in the 2026-11-10 review (one shared deadline per rule, `timeout` honoured, `parallel` bounded wait, last-known-good on a broken file, atomic commit). YAML only: the standalone legacy JavaScript rule format was removed (2026-11-06); scripts that do not parse as rule documents are rejected at creation, and `crabbro rules migrate` wraps an old script as a `run` stage. Inspection scope is designed in [inspect.md](./inspect.md) and is not built. Hook `emits` and `crabbro.pass` are not built.
Date: 2026-11-06
Scope: A rule is match patterns plus a `pipeline` of stages. Plugins are specified in [plugins.md](./plugins.md). The JSON Schema is [schemas/rule.v1.json](../../schemas/rule.v1.json). Inline JavaScript is only a fallback `run` stage.

## What was read

The workcells docs in the agent store (`workcells-ref/`, copied from `lidessen/workcells` main) were readable this time. In particular `docs-actions.md`, `docs-design-actions-redesign.md`, `docs-design-plugins.md`, `docs-design-plugin-installation.md`, `internal-plugin-PROTOCOL.md`, `sdk-plugin-README.md`, `builtin-plugins-decision-README.md`, and `examples-implement-slice.yaml.txt`.

Workcells is a durable orchestration runtime. Crabbro is a hot-path proxy. Shared names are used when the meaning is the same. Durable-run machinery is not copied. The list key is `pipeline`, not workcells' `steps`, because that is the owner's decision for crabbro. Each entry is a pipeline stage.

## 1. File and identity

One rule is one file in `rules/`. The filename without `.yaml` is the `id`, and the file's `id` must match. Files are not nested. A bad file does not invalidate the others. Diagnostics name the file and a path such as `$.request.pipeline[1].if`. They do not repeat values, so a secret in a bad expression is not echoed.

```yaml
version: 1
id: mock-login
description: Synthetic login response
enabled: true
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - id: mock
      uses: crabbro.mock-response
      with:
        status: 200
        headers:
          content-type: application/json
        body: '{"ok":true,"token":"mocked"}'
```

`enabled: false` still loads. It does not run. `crabbro rules preview` counts it separately.

If a live file stops loading (a bad edit), the rule keeps running as it last loaded and `config status` reports the file; deleting the file is what removes it. `config commit` replaces the live file by rename, so a reload never reads half a file. Ids in `config` calls must match `^[a-z0-9][a-z0-9-]{0,63}$`. The 128-file and 8 MiB limits are enforced: files past them are not loaded and are reported.

Drafts sit beside the live file as `mock-login.yaml.draft`. Drafts are not loaded into the proxy. `crabbro config status` lists live files and drafts. `config validate` checks the draft if present, otherwise the live file. `config commit` promotes the draft. `config discard` deletes it. One file is committed even if another file in the directory is invalid.

Limits, from workcells' per-file isolation and tightened for a proxy: 256 KiB per file, 8 MiB and 128 files in the directory. Symlinks, dotfiles, and a file whose name disagrees with `id` are invalid items. They do not hide the valid files.

## 2. What runs where

| | Workcells | Crabbro |
|---|---|---|
| Unit of config | Action, durable Run | Rule, one exchange. No Run log beyond the existing request trace and SQLite history. |
| List key | `steps` | `pipeline` |
| Call | `uses: task.get` plus `instance` | `uses: protobuf.decode` plus `instance` |
| Inline script | `run:` is literal just-bash. `${{ }}` is not expanded into the script. Dynamic values use stdin. | `run` is literal Goja. `${{ }}` is not expanded into the script. Inputs arrive as a frozen object. |
| Branch | `select`, then `branch:` on later steps, then `join` | `select` whose cases contain their own `pipeline`. No scattered `branch:` and no `join`. |
| Parallel | Not in the current runtime. The redesign wants parallel paths that join. | `parallel` is observe-only and does not join. |
| Queue | `concurrency: {mode: queue}` | Not used. A proxy does not queue the client. |

A rule compiles to one middleware in the onion (`ruleMiddleware`). Priority, then `id`, decides order. Lower runs first on the way in. Response pipelines unwind in reverse. Stages of two rules are not interleaved.

`crabbro.*` stages are Go. They do not start Node. A plugin stage starts that plugin's worker on first use and reuses it.

## 3. Stages

An action stage:

| Field | Meaning |
|---|---|
| `id` | Required if a later stage reads `pipeline.<id>.outputs`. |
| `uses` | `crabbro.<capability>` or `<plugin>.<capability>`. |
| `instance` | Default `local`. May be a whole-value expression. Empty or non-string fails the stage. It does not fall back to `local`. The resolved name is what the trace records. |
| `with` | Inputs. Each value is a literal or a whole-value `${{ }}`. No embedded expressions in `with`. Build text in `vars` first, and `vars` cannot see stage outputs, so a header that must be composed is returned whole by the plugin. |
| `env` | Strings, numbers, or bools, or a whole-value expression that resolves to one of those. Not a secret store. Max 32 entries. `select` and `parallel` do not take `env`. |
| `middleware` | Explicit list of `{plugin, instance, name}`, at most 8. Registering the plugin does not apply it. |
| `if` | Skip when false. |
| `continueOnError` | Default false. A failed stage stops the rest of that pipeline. |
| `timeoutMs` | Default is the rule's `timeout` (500 ms unless the document says otherwise, max 2 s). Every stage of a rule shares one deadline, the exchange's remaining 2 s budget; a stage is cut to what is left of it, and a stage that starts with none left fails with `rule evaluation budget exceeded`. |

`vars` are read-only, resolved once before the first stage, and stored on the trace. They may reference `request.*` and other vars. They may not reference `pipeline.*` outputs or secrets. Cycles fail at validate time. Limits: 64 vars, 64 KiB, depth 16. A whole-value expression keeps its JSON type. An expression embedded in a string becomes text. Lists and objects have no text form and are rejected. Secrets never go in `vars`. The trace can contain vars, so they are not credentials.

Built-ins:

| `uses` | Phase | Effect |
|---|---|---|
| `crabbro.tag` | request | mutate |
| `crabbro.set-request-header` | request | mutate |
| `crabbro.remove-request-header` | request | mutate |
| `crabbro.rewrite-url` | request | mutate |
| `crabbro.gateway-offload` | request | mutate. Terminates forwarding and sends a sanitized request to the local gateway using `with.protocol` (`responses` or `anthropic-messages`) and `with.model` (a gateway route model or `plan/model`). Errors do not fall back to the original upstream. |
| `crabbro.mock-response` | request | mutate. Later stages in that pipeline do not run. The response pipeline of this rule still runs on the synthetic response. |
| `crabbro.block` | request | mutate. Same short-circuit. |
| `crabbro.abort` | request | mutate. HTTP 403. |
| `crabbro.set-response-header` | response | mutate |
| `crabbro.remove-response-header` | response | mutate |
| `crabbro.set-response-status` | response | mutate |
| `crabbro.rewrite-response-body` | response | mutate |
| `crabbro.delay` | response | mutate. It changes when the client is released. |
| `crabbro.throttle` | response | mutate. Same reason. |
| `crabbro.replace-frame` | frame | mutate |
| `crabbro.drop-frame` | frame | mutate |
| `crabbro.read-body` | request or response | observe. Outputs `bytes` and `truncated`. |
| `crabbro.log` | any | observe |
| `crabbro.metric` | any | observe |
| `crabbro.notify` | any | observe |
| `crabbro.tee` | any | observe. Copies the snapshot, including at most the body preview. |

`mock-response` and `block` in one pipeline are a validation error. A plugin action without `effect` is `mutate`.

## 4. Examples

### Mock a login

The example in section 1. No plugin worker starts.

### Rewrite a response field

```yaml
version: 1
id: hide-internal-role
on:
  request:
    host: api.example.com
    pathPrefix: /v1/users
response:
  pipeline:
    - uses: crabbro.rewrite-response-body
      with:
        find: '"role":"internal"'
        replace: '"role":"member"'
    - uses: crabbro.set-response-header
      with:
        name: X-Crabbro
        value: role-rewritten
```

Bodies over 256 KiB (`MaxTransformBody`) are left unchanged.

### Decode protobuf, then select a path

```yaml
version: 1
id: decode-login-body
on:
  request:
    method: POST
    host: api.example.com
    path: /v1/login
request:
  pipeline:
    - id: decoded
      uses: protobuf.decode
      instance: local
      with:
        message: login.LoginRequest
        encoding: protobuf
    - id: route
      select:
        cases:
          - name: password
            if: pipeline.decoded.outputs.kind == "password"
            pipeline:
              - uses: crabbro.tag
                with:
                  tags: ["password-login"]
          - name: sso
            if: pipeline.decoded.outputs.kind == "sso"
            pipeline:
              - uses: crabbro.set-request-header
                with:
                  name: X-Login-Kind
                  value: sso
        default:
          pipeline:
            - uses: crabbro.tag
              if: pipeline.decoded.outputs.ok == true
              with:
                tags: ["other-login"]
    - parallel:
        timeoutMs: 200
        stages:
          - uses: crabbro.log
            with:
              message: decoded-login
          - uses: crabbro.metric
            with:
              name: login_decoded
```

`select` replaces the earlier `switch`. The arm's stages sit inside the case, which is what workcells' redesign wants and what its current `branch:` annotations do not guarantee. A forgotten `branch:` on a later step would mutate live traffic. That is unacceptable on a proxy, so crabbro does not use `branch:` or a separate `join`. The taken arm's stage outputs are in scope for later stages of this pipeline. Outputs of untaken arms are not. Referencing one fails the rule.

The first matching case wins. A condition error fails the rule. It is not treated as false and it does not fall through to `default`. The chosen case, and the skipped state of the other arms, is written to the trace before any stage in an arm runs.

### Sign a request

```yaml
version: 1
id: sign-upstream
on:
  request:
    method: POST
    host: api.partner.test
    pathPrefix: /v1/
vars:
  host: api.partner.test
request:
  pipeline:
    - id: signed
      uses: hmac.sign
      instance: primary
      with:
        secret: "${{ secrets.partner_hmac }}"
        algorithm: sha256
    - uses: crabbro.set-request-header
      if: pipeline.signed.outputs.ok == true
      with:
        name: X-Signature
        value: "${{ pipeline.signed.outputs.header }}"
```

`with.value` is a whole-value expression, so it keeps the string type. The plugin returns the finished header. The rule does not concatenate. `secrets.partner_hmac` is a credential ref on the `hmac/primary` instance. The host injects it. It is not a var.

## 5. Branching and parallel

`if` may appear on an action, a `select`, or a `parallel` block. False skips that stage. The trace records `skipReason: if`. A skipped stage publishes no outputs.

`select` is the multi-way branch. It is not a parallel fan-out. No match and no `default` skips every arm (`skipReason: unselected`). Nested `select` is rejected, matching workcells' current "no nested select". A case pipeline may contain actions and `parallel`, not another `select`.

`parallel` is only for `effect: observe`. It does not join results, which is the opposite of the parallel block in workcells' redesign. That redesign is for a durable run that waits for lint and tests. Crabbro's block runs its stages side by side on a snapshot of the view and of the outputs of stages that already finished, **waits for them up to its `timeoutMs`**, then continues. The wait is bounded by the block's `timeoutMs` (default: the rule's `timeout`) and by what is left of the rule's deadline, so a block can hold the client for at most that long. A stage still running at the deadline is abandoned, traced as failed, and cannot write into the block's result. Stages inside the block do not see each other. Later stages cannot read `pipeline.<id>.outputs` for an id declared inside the block. Those outputs are trace-only.

`timeoutMs` ends the wait. Timeout and error never fail the exchange and never trigger `fail-closed`. `delay` and `throttle` are mutate because they change when the client sees bytes. `tee` copies the snapshot, at most the body preview, not the live stream.

If the agent needs "notify, then forward only if it succeeded", that notify is a normal sequential stage.

## 6. Expressions

Allowed roots: `request`, `response`, `frame`, `pipeline.<id>.outputs`, `vars`, `hook.outputs`, `secrets` (whole value only).

`if` and `select` case conditions use `==`, `!=`, `&&`, `||`, and parentheses. No calls and no arithmetic. A condition error fails the rule.

`with` and `env` values are literals or a single whole-value expression. Embedded expressions are not accepted there. Concatenate in `vars` when the pieces are known before the pipeline starts. Pieces that come from an earlier stage must be returned as one value by that stage.

## 7. Hooks and phases

1. Match `on.request` in Go. Do not start a worker if every matched stage is `crabbro.*` or `run`.
2. Activate the instances those stages name.
3. Request phase, outer middleware to inner. Each rule runs `request.pipeline`. A stage's `emits` runs subscriber rules before the next stage. Depth one. Cycles fail at validate time.
4. Upstream, unless mock, block, or abort short-circuited.
5. Response phase, inner to outer.
6. WebSocket data frames use `frame.pipeline` of rules whose request phase matched the upgrade. Ping, pong, and close are not a phase.

`crabbro rules preview --request <id> --json` runs the matcher and reports which rules would run. It does not create an exchange and does not call plugins. The same operation is the tool `rules.preview`. `rules.diagnose` lists files that failed to load without echoing values. `complete: false` means at least one file is unreadable, so a match result may be partial.

## 8. Validation and migration

`crabbro rules validate <file>` checks [schemas/rule.v1.json](../../schemas/rule.v1.json), then the walker: expression grammar, phase versus `uses`, observe-only `parallel`, no reference to parallel outputs, installed instance, `with` against the action schema, terminal-action exclusivity, and hook cycles. `--offline` skips the instance lookup. Field paths look like `request.pipeline[2].select.cases[0].pipeline[0].uses`.

`crabbro rules from <id>` emits this YAML, not JavaScript. `--uses protobuf.decode` emits a stage whose `with` keys come from that action's schema.

Existing JS rules remain loadable as one `run` stage. `crabbro rules migrate <file.js>` prints that wrapper. It does not recover `uses` from arbitrary JS. New authoring is YAML. `run.runtime` must be `goja`. The script is literal. `run` is `mutate`, so it cannot sit in `parallel`.

## 9. What was not taken, and why

| Workcells | Crabbro choice |
|---|---|
| `steps` | `pipeline`. Owner's name. |
| `select` plus `branch:` on each later step plus `join` | `select` with the pipeline inside the case. Workcells' own redesign wants stages inside the branch for the same reason: a missed annotation runs the stage on the wrong path. On a proxy that path mutates a live request. `join` is unnecessary because the taken arm is inline. |
| Parallel paths that join | Not taken. That waits. Observe-only `parallel` does not. |
| `concurrency` queue, cron, misfire, durable Run store, exactly-once | Not taken. The client is waiting. History already stores the exchange. |
| `run` as just-bash, dynamic values on stdin | `run` as Goja. Same rule: the script text is not an expansion site. |
| ScriptC native binary per plugin | Not taken. Crabbro's host is Node, one process, a worker per active plugin. |
| Human text by default, `--json` for machines | `--json` is accepted and is the default. `--pretty` is the human form. Agents are the primary operator. The shared part is the contract: bounded output, stable field names, diagnostics that do not echo values. |
| Decision locked to `api.commandcode.ai` and Keyref | The sink pattern is taken (host calls, credential ref, no key in the rule, `decisionDiagnostic`). The endpoint may be Jev or OpenAI-compatible. |
| Task, delegate, Herdr, resources, MCP | Not part of the proxy. |

## 10. Critique

Structured `select` is safer than both a flat `switch` on one string and workcells' current scattered `branch` tags. The cost is that an exclusive branch cannot share a trailing stage without duplicating it or placing it after the `select`. Duplication is the right default. A stage after the `select` runs for every arm that did not short-circuit, which is the join-by-falling-through that `join` made explicit. Authors who need "only the taken arm's object" should read a stage id that exists only in that arm and accept the failure if they reference the other arm.

`parallel` without a join will be misused by people who learned the word from CI. The validator has to reject mutate stages and references to parallel outputs, or the feature becomes a race. Non-blocking also means a failed notify is invisible to the client. That is acceptable only because the trace still records it.

`with` accepting only whole-value expressions will annoy agents who want `` `user ${{ pipeline.decoded.outputs.userId }}` ``. Forcing the plugin to return the finished string is the workcells rule and the right one here: string assembly inside `with` is how a secret gets concatenated into a header and then into a trace. `vars` cannot see stage outputs, so they are not a back door.

Per-file isolation fits a proxy better than one big rules file. One bad YAML must not stop the rest of traffic. The matching gap is real: `rules.diagnose` with `complete: false` means a request might have missed a rule that failed to parse. The CLI has to say that. It must not pretend the match set is complete.
