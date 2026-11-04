# Inspecting traffic

Status: Implemented, except the deferred items at the end of this file.
Date: 2026-11-04
Scope: How an agent finds the one app or page it means to change when the proxy is seeing many of them at once.

The proxy fills `Request.Attribution` on each recorded exchange. Session scope and durable allow/deny lists are loaded. A browser extension or Chrome DevTools Protocol is not connected, so tab ids and initiator scripts stay empty unless a client sends `X-Chiling-Page` or `X-Chiling-Initiator`. The proxy strips those stamps before forwarding.

## What the agent is doing

The machine is running a browser with many tabs, a few Electron or native apps, and background updaters. The agent can list those sources, read enough about each one to tell them apart, and mute the ones that are not the target. Muting is a view, not a rewrite. The agent then narrows inside the remaining page: this script's calls, not the images, not the analytics script. When the session ends, that narrowing is gone. Hosts and apps the operator never wants to look at stay on a durable deny list.

## What a proxy can know

Chiling is a local forward proxy. For each accepted TCP connection it knows the peer address and, once the client speaks HTTP, the request line and headers. That is the whole ground truth.

### Process and app

The HTTP bytes do not name the process. The proxy learns it by asking the OS which PID owns the local port on the connection it just accepted, and it must do that before a short-lived client such as `curl` exits.

| Platform | Lookup | What it returns |
|---|---|---|
| Linux | Read `/proc/net/tcp` (and `tcp6`) for the inode of the 4-tuple, then scan `/proc/<pid>/fd` for that socket inode. `comm`, `exe`, and the cgroup name come from that pid. | PID, executable, command name |
| macOS | `proc_pidinfo` / `proc_pidfdinfo` for the listening peer, or the same data `lsof -nP -iTCP:<port>` shows. A full `lsof` walk is the fallback, not the hot path. | PID, path to the binary, often the app bundle |
| Windows | `GetExtendedTcpTable` with `MIB_TCPTABLE_OWNER_PID`, then `QueryFullProcessImageName`. | PID, image path |

`SO_PEERCRED` does not apply. That socket option is for Unix sockets. The proxy's client connections are TCP, including to `127.0.0.1`.

Limits, stated plainly:

- The lookup sees processes of the same user. It is not a machine-wide view of other users.
- Chrome, Edge, and Firefox move sockets into a network-service or socket process. The PID on the connection is that helper, not "the browser" and not a tab. The proxy walks the parent chain and rolls a `--type=utility` or `--type=gpu` child up to the ancestor whose executable is the app. The recorded source is the app (`Google Chrome`), with the helper PID kept as a detail. Two browsers that share a name are split by executable path.
- A process that exits before the lookup runs is `kind: unknown`. The exchange is still captured.
- iOS and Android are out of this design. A per-app VPN or network extension is a different product.
- QUIC is UDP. A classic HTTP proxy does not see it. Chromium disables QUIC when an HTTP proxy is configured and uses CONNECT instead, which is the path this design covers. A browser that speaks QUIC beside the proxy is invisible. The source list must not pretend otherwise.

### Page, tab, and frame

Process attribution stops at the app. A pure proxy cannot see tab ids, frame ids, or document titles.

What it can infer, weakly, from headers the browser already sends:

| Header | Use | Limit |
|---|---|---|
| `Origin` | Origin of a CORS `fetch` / XHR | Absent or `null` on many navigations and simple requests |
| `Referer` | Previous document URL | Removed or trimmed by `Referrer-Policy`. It is not a tab id. Two tabs on the same URL are one page. |
| `Sec-Fetch-Site` | `none`, `same-origin`, `same-site`, `cross-site` | Coarse. No tab. |
| `Sec-Fetch-Mode` | `navigate`, `cors`, `no-cors`, `websocket`, `nested-navigate` | Distinguishes a document navigation from a subresource |
| `Sec-Fetch-Dest` | `document`, `script`, `style`, `image`, `font`, `empty`, `worker`, `iframe`, … | Best signal a proxy has for resource type. `empty` is XHR/fetch. |
| `Sec-Fetch-User` | `?1` on a user navigation | Not an identity |

The page key a proxy can form by itself is `origin` plus, when `Sec-Fetch-Dest` is `document`, the request URL, and otherwise the `Referer` if it is present. Confidence is `header`. Service workers, extension pages, and `Referrer-Policy: no-referrer` leave the document URL empty. The agent still has an origin and a resource type.

### Initiator script

A pure proxy does not know which script called `fetch`. `Referer` is the document, not the caller. Stack traces are not on the wire.

Two optional integrations can supply it. Both are explicit, neither is required for the proxy to run, and both are later work:

- A browser extension (MV3) knows `tabId`, `frameId`, and the document URL. It stamps a request header such as `x-chiling-page` and `x-chiling-initiator` that the proxy strips before the bytes go upstream. The origin must not see the stamp. The extension talks to the daemon only as a hint; the proxy remains the source of the exchange id.
- Chrome DevTools Protocol (`Network.requestWillBeSent`) has `initiator.url`, the script line, `frameId`, and a resource `type`. Attaching requires a browser launched with a debugging port. Matching a CDP event to a proxy exchange by method, URL, and time is fuzzy when many identical requests are in flight. The extension stamp is the reliable join. CDP is the way to get the initiator script when the stamp only carries the tab.

Confidence on a page or initiator is `header`, `extension`, or `cdp`. The CLI prints it. An agent must not treat `header` as a tab.

## Data model

These objects are not a second store. They hang off the exchange the proxy already records, plus two small indexes in memory for the life of the daemon process (sources and pages). History keeps the snapshot that was known when the exchange finished, inside the existing JSON payload, so old rows simply lack the object.

```text
source
  id            src_<n>
  kind          app | browser | cli | unknown
  name          "Google Chrome" or "curl"
  executable    absolute path, when the OS gave one
  pid           number, 0 if unknown
  helperPid     the socket owner when it differed from the app
  platform      linux | darwin | windows

page
  id            page_<n>
  sourceId
  origin        https://app.example.com
  url           document URL, possibly empty
  title         empty unless extension or CDP set it
  tabId         empty unless extension or CDP set it
  frameId       empty unless extension or CDP set it
  confidence    header | extension | cdp

initiator
  type          script | parser | other | unknown
  url           script URL, empty when unknown
  line          0 when unknown
  confidence    cdp | extension | none

resourceType    document | xhr | fetch | script | stylesheet | image
                | font | media | websocket | worker | other | unknown
```

`xhr` and `fetch` are split when the client tells us (`Sec-Fetch-Mode: cors` plus a later extension/CDP type). From headers alone, `Sec-Fetch-Dest: empty` is recorded as `xhr` and `confidence` stays `header`, because the proxy cannot see the JavaScript API name. CDP's `type` replaces it when present.

The reserved `Request.Attribution` struct is the on-exchange hook: `sourceId`, `pageId`, `initiatorUrl`, `resourceType`, `confidence`. Implementing this document means filling that struct, not inventing a parallel record.

## Two lifetimes

### Session scope

Session scope is the REPL. It is attached to the open recording session in `internal/session`. It is not written to disk. `record stop` drops it. A daemon restart drops it. Closing a CLI process does not drop it, because the CLI is a client and the next command must see the same scope. "Forgotten on exit" means the recording session ended, not that one short-lived `chiling` process exited.

If the operator has not run `record start`, the first `scope` command opens an implicit recording session named `scope`. `record stop` forgets that too. There is no second, hidden scope store.

State:

```text
pick    []expr     OR. Empty means "no positive filter".
omit    []expr     OR. Any hit excludes the exchange from the default view.
bypass  []expr     explicit only. See below.
```

An exchange is in the default view when all of these hold:

1. No durable deny entry matches.
2. If `pick` is non-empty, at least one pick matches.
3. No session omit matches.

`scope pick a b` replaces the pick list. `scope omit c` appends. `scope off c` is `omit`. `scope on c` removes `c` from omit and, if a pick list exists and `c` is a specific source or page id, adds it to the pick list. `scope reset` clears pick, omit, and session bypass. It does not touch the durable lists.

Expressions, all of them whole tokens, combined only by repeating the command (OR inside pick, OR inside omit):

```text
app:<name>            substring match on source name, case-insensitive
pid:<n>
origin:<origin>       exact origin
page:<pageId>
host:<pattern>        same host patterns the MITM matcher already uses
pathPrefix:<path>
type:<resourceType>
initiator:<url>       exact script URL
```

Unknown tokens are a validation error. The CLI does not echo a rejected token back if it could be a secret in a path; it names the position (`expr[1]`) and the reason (`unknown key`).

### Durable allow and deny

Long-term exclusion lives in `~/.chiling/scope/`, one file per list, filename equal to id:

```text
~/.chiling/scope/deny.yaml
~/.chiling/scope/allow.yaml
~/.chiling/scope/deny.yaml.draft
```

```yaml
version: 1
id: deny
entries:
  - expr: host:*.google-analytics.com
    action: hide
  - expr: app:Spotify
    action: hide
```

`action` is `hide` or `bypass`. Drafts use the same `config draft`, `validate`, `commit`, and `discard` flow as rules: a `.draft` file is not loaded, commit promotes one file even if another file in the directory is invalid, diagnostics name the file and a path and do not repeat values. Symlinks and a filename that disagrees with `id` are skipped and reported. A bad file does not unload the other list.

Empty `allow` means no extra restriction. This is deliberate and different from MITM. A published MITM allowlist treats empty as "decrypt nothing", because decryption is opt-in. Scope allow treats empty as "do not narrow the view", because the agent starts from the full firehose and subtracts. Deny always subtracts. A session pick cannot resurrect a config deny. `requests list --all` still shows the row, marked hidden by config, so the agent can see the exclusion.

## What "off" does to the bytes

Three behaviors exist. Only one is the default.

| | Forwarded | MITM | Recorded | In `requests list` | Rules run |
|---|---|---|---|---|---|
| In scope | yes | policy | yes | yes | yes |
| Session omit or config `hide` (default) | yes | policy | yes, flagged hidden | no, unless `--all` | yes |
| Session or config `bypass` (explicit) | yes, CONNECT tunnel | no | no | no | no |

View filters do not change forwarding and do not disable rules. The agent is trying to see less. Dropping images or skipping a rule because the view is narrow would change the app under test and would make `verify` lie. Hidden exchanges stay in the ring and in SQLite with `hidden: true` and `hiddenReason` (`session-omit`, `config-deny`, or both). `requests get <id>` works for a hidden id. The list response carries `hiddenCount` so the agent can tell that the firehose was reduced rather than that the app went quiet.

`bypass` is the only way to skip MITM and rules. It is never implied by `omit` or `off`. It exists for flows the operator does not want to decrypt (a noisy updater, a pinned-cert app). `scope status` lists bypass entries next to the view filters so they cannot be mistaken for a mute.

## Rules

A rule's `on.request` is independent of the view. Scope does not silently disable a rule. `rules preview` reports the match either way and adds `inScope: false` when the current view would hide that exchange. The agent can still `verify replay` a hidden id.

A future opt-in field, not in the rule schema now, would be `scope: session` on a rule: run only when the exchange is in the current view. It stays opt-in so a rule checked in last week cannot change meaning because the agent muted a tab. This design does not add that field.

MITM allow and deny stay the decryption policy. A host can be decrypted, recorded, hidden from the list, and still rewritten by a rule. Bypass is a separate, explicit "do not intercept" and is printed as such.

## CLI and JSON

JSON is the default, as on every other command. `--pretty` is the human form. `--json` is accepted and changes nothing.

```text
chiling sources list
chiling sources get <id>
chiling scope status
chiling scope pick <expr>...
chiling scope omit <expr>...
chiling scope off <expr>...
chiling scope on <expr>...
chiling scope reset
chiling scope bypass <expr>...      # explicit; not a view mute
chiling requests list               # default view
chiling requests list --all         # includes hidden rows
```

Durable edits go through the config commands that rules already use:

```text
chiling config draft ~/.chiling/scope/deny.yaml
chiling config validate deny
chiling config commit deny
chiling config discard deny
```

`sources list` counts what the current session has seen:

```json
{
  "sources": [
    {
      "id": "src_3",
      "kind": "browser",
      "name": "Google Chrome",
      "executable": "/usr/bin/google-chrome",
      "pid": 4412,
      "seen": 180,
      "inScope": 12,
      "hidden": 168
    }
  ]
}
```

`scope status`:

```json
{
  "sessionId": "ses_4",
  "pick": ["app:Google Chrome", "origin:https://app.example.com"],
  "omit": ["type:image", "type:font", "type:stylesheet", "initiator:https://app.example.com/analytics.js"],
  "bypass": [],
  "configDeny": ["host:*.google-analytics.com"],
  "configAllow": []
}
```

`requests list` in a narrowed session:

```json
{
  "requests": [],
  "hiddenCount": 40,
  "scopeActive": true,
  "nextCursor": ""
}
```

A hidden row under `--all` adds `"hidden": true` and `"hiddenReason": ["session-omit"]`. `sources get` includes the pages grouped under that app and the same confidence field.

The control API mirrors this under `/v1/sources`, `/v1/scope`, and the existing `/v1/requests` with `hiddenCount`. Errors use the existing envelope.

## An agent session

The daemon is already proxying the desktop. The agent has not picked a target.

```text
chiling record start --name checkout-bug
chiling sources list
```

The list shows Chrome (about 200 exchanges), a music app, `curl` from a health check, and an updater. The agent reads `sources get` for Chrome and sees pages for `https://mail.example`, `https://app.example.com`, and a docs site.

```text
chiling scope off app:Spotify app:Updater pid:9021
chiling scope pick app:Google Chrome origin:https://app.example.com
chiling scope omit type:image type:font type:stylesheet type:media
chiling requests list
```

`hiddenCount` is most of the original firehose. The remaining rows are documents, scripts, XHR, and websockets for that origin. One script URL shows up only if an extension or CDP is attached; otherwise the agent groups by path and `type:xhr`.

```text
chiling requests list --pathPrefix /v1/
chiling scope omit initiator:https://app.example.com/static/analytics.js
chiling requests get req_18
```

`req_18` is `POST /v1/checkout`. The agent drafts a rule with `on.request` for that host and path. The rule runs on the next checkout even if the agent later mutes the page, because scope did not attach itself to the rule. `rules preview --request req_18` reports the match and `inScope`. `verify replay req_18` checks the rewrite.

```text
chiling record stop
```

The pick and omit lists are gone. `host:*.google-analytics.com` on the committed deny list is still hidden tomorrow.

## Trajectory UI

`internal/session` already stores a timeline. Scope commands append activity on the current session:

| Kind | When |
|---|---|
| `scope_picked` | `scope pick` replaced the pick list |
| `scope_omitted` | `scope omit` or `scope off` |
| `scope_restored` | `scope on` |
| `scope_reset` | `scope reset` |
| `scope_bypass` | an explicit bypass was added or removed |
| `scope_committed` | a durable allow/deny file was committed |

The summary is a short sentence ("omitted type:image, type:font"). The expressions are the activity detail. Bodies and headers are not copied into the timeline.

A later UI renders three things from data this design already names:

- A chip row above the request list for each pick and omit, with the hidden count beside it. Clearing a chip calls `scope on` or `scope reset`.
- A sources column: app name, executable, seen / hidden. A mute control calls `scope off app:<name>`.
- Timeline markers at the scope activity entries, so a replay of the session shows when the agent stopped looking at the other tabs. The request rows themselves stay the exchanges. Hidden ones are behind a "show hidden" toggle that is `requests list --all`.

The UI does not keep a private filter. It reads `GET /v1/scope` and `GET /v1/sources`.

## Room left in the current tree

- `Request.Attribution` is the on-record hook. It is omitempty and unset.
- Activity kinds are not an enum, so `scope_picked` and the rest can be recorded without a migration.
- `requests list` can grow `hiddenCount` and `hidden` without removing today's fields.
- Rules stay matched by `on.request` only. No code path consults a view filter, which is the behavior this design wants as the default.
- Durable scope files are specified to reuse `config draft/validate/commit/discard` rather than a new write protocol.

## Decisions

- Any `scope` command with no recording session opens an implicit session named `scope`. The JSON includes `implicitSession: true`. `record stop` forgets the session scope.
- `Sec-Fetch-Dest: empty` is resource type `xhr`. A request with no Sec-Fetch-Dest header at all is `unknown`.
- A failed PID lookup is `kind: unknown`. The exchange is still proxied and recorded.
- Empty durable allow does not narrow. `scope status` reports `restricting: false` in that case.
- Session bypass and durable `action: bypass` are both explicit. `omit` and `off` only hide.

## Deferred

- Browser extension and Chrome DevTools Protocol. Without them the proxy has no tab id and no initiator script unless the stamp headers are already present.
- macOS uses `lsof` for the port owner. `proc_pidfdinfo` is not linked in this build. Windows calls `GetExtendedTcpTable`. Both files cross-compile. Only Linux is exercised here.
- A bypass that names a resource type or initiator is decided after the request is visible, so a connection that was already decrypted stays decrypted. Bypass of an app, pid, or host on `CONNECT` skips MITM and does not record the tunnel.
- Hide reasons include `session-pick` and `config-allow` in addition to `session-omit` and `config-deny`, so a pick miss is distinguishable from an omit.
- `config validate allow` and `config validate deny` edit the scope directory, not a rule with those ids.
