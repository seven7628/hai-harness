# Panel API / host contract

Part of `create-mode`. Read `SKILL.md` first; this file is loaded only when you are
writing `surface.json` / `panel.js` or debugging rendering/lifecycle.

## surface.json fields

The installer and loader read this exact set. Unknown JSON keys are ignored by Go, so a typo does
**not** fail loudly — only the fields below have meaning.

| field | type | required | meaning |
|---|---|---|---|
| `id` | string | yes | `^[a-z][a-z0-9-]{1,40}$`; also the storage prefix |
| `name` | object | recommended | language → display name, e.g. `{"zh":"成本看板","en":"Cost board"}`. A **plain string fails to unmarshal** (the installer reports it as “surface.json 不是合法 JSON”). Missing name falls back to `id`. |
| `version` | string | yes | non-empty; use semver (`1.0.0`) so upgrades read naturally |
| `kind` | string | yes | exactly `plugin:<id>` |
| `entry` | string | no | entry file inside the folder; default `panel.js` |
| `icon` | string | no | path inside the folder; provider fallback icon |
| `commands` | string[] | no | host commands the plugin may call (see below) |
| `data` | string[] | no | event types the plugin may subscribe to |
| `entries` | object[] | no | declared pages, ≤ 20; see [`entries.md`](entries.md) |
| `tools` | object[] | no | model-callable tools, ≤ 16; see [`tools.md`](tools.md) |

`api` and `placement` are **not part of the contract**: the current template no longer ships them.
If an old plugin still carries them, they are ignored; do not add them to new plugins.

## panel.js rules (the hard ones)

- **No JSX, no bundler.** `panel.js` is the file the browser imports; there is no compile step.
  Use `React.createElement` (aliased `h`). Anything you would `import` from npm will fail — React
  arrives as an argument (`apply({ host, React, surfaces })`).
- **Export exactly one thing**: `export const surface = { kind, apply({ host, React, surfaces }) }`.
- Register **your own kind only**, once, inside `apply`:
  `surfaces.register({ kind: 'plugin:x', group: 'plugin', title: () => '…', render: () => h(Panel) })`.
  Optional provider fields: `order?: number` (menu/settings ordering), `icon?: (tab?) => ReactNode`,
  `dedupeKey?: (tab) => string` (default = `JSON(tab.params ?? {})`), and `badge?: (tab, state) => number`.
  Keep the provider's `kind` equal to the manifest's; the host rejects a mismatch.
- **Two functions, that is the whole API**: `host.command(name, payload)` and
  `host.onEvent(type, fn)`. There is no `host.agent`, no `host.composer`, no `window.desktop`,
  no module you can import the host from. (Older examples used `host.events.on`,
  `host.openTab`, `host.toast` — those are gone.)
- Read/write the world only through `host.command`, and declare every name in
  `surface.json.commands`. Everything the host offers a plugin:

  | command | payload | reply |
  |---|---|---|
  | `agent.session` | `{title?}` | `{ok, session_id}` |
  | `agent.run` | `{session_id?, input, instructions?}` | `{ok, run_id, session_id}` — the **content** arrives as events |
  | `agent.cancel` | `{run_id}` | `{ok}` |
  | `agent.snapshot` | `{session_id}` | `{ok, session_id, status, seq, run_id?, started_at?, last_error?, quota}` — only your own session; never content |
  | `tool.result` | `{call_id, ok, result?, error?}` | `{ok}` — only for plugin tools (P2) |
  | `composer.quote` | `{text, source?}` | `{ok}` — inserts into the current draft, **never sends** |
  | `ui.open_tab` | `{kind, params?}` | `{ok}` — `kind` must be your own |
  | `ui.toast` | `{message}` | `{ok}` |

  Replies are **values, never exceptions**. Check `ok` and render the reason; the host uses this
  closed set (do not invent alternatives):

  | reason | meaning |
  |---|---|
  | `not_declared` | the name is missing from `surface.json.commands` |
  | `denied` | the host refused (D11, wrong kind, no channel, missing capability) |
  | `invalid_input` | malformed/empty payload, or quote over 8 KiB |
  | `no_session` | the session id does not exist, or no active composer to quote into |
  | `error` | transport/agent failure; no internal detail is exposed |
  | `panel_not_open` | a `requires_panel` tool fired while no tab of your kind is open |
  | `timeout` / `plugin_unloaded` | tool call missed its deadline, or the plugin went away |

  `agent.run` returns the flat fields (`run_id`, `session_id`) **and** the same values under
  `data` for wire compatibility. Read the flat fields; do not branch on both shapes.
- **The four old read-only commands are disabled** (D11): `workspace_list`, `session_list`,
  `git_snapshot`, `ui_inspect`. They can read real project/session metadata, which a panel must not
  touch. Declaring them makes **installation `rejected`** (D11); old installed plugins that still
  declare them answer `denied` at runtime. Copy the current `templates/surface/`, do not copy old
  examples.
- Subscribe only to event types declared in `surface.json.data` — `host.onEvent` **throws** for an
  undeclared type (so a typo fails at `apply` time instead of silently never firing). The host
  **tracks every subscription and disposes it on reload/unload**; `off()` is for removing one early.
  Subscribe in `apply` when the listener must keep serving while any tab is closed (tool calls
  without `requires_panel`); subscribe in a component's `useEffect` only when you deliberately want
  it tied to that panel's mount:

  ```js
  const off = host.onEvent('agent.delta', (ev) => setText((t) => t + ev.data.text))
  // off() removes it early; plugin unload removes it automatically.
  ```

  | event | data | notes |
  |---|---|---|
  | `agent.delta` | `{text}` | incremental text (host batches ~50 ms) |
  | `agent.tool` | `{name, status, ok?}` | tool name + status only |
  | `agent.usage` | `{input_tokens, output_tokens}` | once, near the end |
  | `agent.done` | `{text}` | terminal |
  | `agent.error` | `{reason, message?}` | terminal |
  | `agent.state` | `{session_id, status, seq, run_id?, reason?, message?}` | session-level transition; also fires when the **user** drives your plugin session from the left panel |
  | `tool.call` | `{call_id, tool, args, deadline_ms, read_only, requires_panel}` | the model wants **your** tool; reply with `tool.result` |
  | `surfaces_changed` | `{}` | plugin installed/enabled/removed (also spelled `surfaces.changed`) |

  `surfaces_changed` needs no token and works even when the agent channel is unavailable; every
  other event only ever carries **your own** runs (the loader filters by plugin id).

- **Your own session is readable as state, never content.** Declare `agent.snapshot` in
  `commands` and `agent.state` in `data` when the panel shows whether its session is running:

  ```json
  {"commands":["agent.session","agent.run","agent.snapshot"],
   "data":["agent.delta","agent.done","agent.error","agent.state"]}
  ```

  ```js
  const r = await host.command('agent.snapshot', { session_id: sessionId })
  // r = {ok, session_id, status:'idle'|'running'|'not_loaded', seq, run_id?, started_at?,
  //      last_error?:{reason,message?}, quota:{runs_left_this_hour, concurrent_limit, run_timeout_ms}}

  const offState = host.onEvent('agent.state', (ev) => setStatus(ev.data.status))
  ```

  `agent.state` also fires when the **user** opens the plugin session from the left panel and
  types there — that run is not initiated by `agent.run`, so the run-projection events
  (`agent.delta` / `agent.done`) do not carry it. `seq` here is **session-level**; do not compare
  it with the per-run `seq` on `agent.delta`. Only the plugin that owns the session can snapshot
  it: a project/Hub session or another plugin's session is `denied`, an unknown id is
  `no_session`; there is no implicit “current session” and no content/messages in the reply.
  The snapshot is capped at 2 KiB; `last_error.message` is clipped by the host.

- Theme from `host.theme()` (`{ mode, tokens: { bg, fg, border, accent, ok, warn, err, info } }`).
  Hardcoded colours break one of the two themes.
- Opening another tab is `host.command('ui.open_tab', { kind: 'plugin:<your-id>', params })` —
  your own kind only. A one-line message is `host.command('ui.toast', { message })`. Both are
  conveniences: a panel that only renders is a fine panel.
- Quote into the draft with `host.command('composer.quote', { text, source })`: the text lands in
  the user's composer, `source` becomes a deletable chip on it, and **nothing is sent** (the user
  presses Enter). `text` is capped at **8 KiB of UTF-8 bytes** (a 3-byte CJK char counts 3);
  empty/oversized text is `invalid_input`, no active session is `no_session`. `source` is either a
  string or `{ id?: string, title?: string }`; the host keeps at most 200 chars of each and falls
  back to `id` when `title` is absent (an unknown shape is dropped, not an error).

## Host runtime contract

Four questions every plugin hits while it is being written; the answers are not visible from the
rest of this file.

**Where your code runs.** `panel.js` is imported into the app's own page — not an iframe, not a
worker. You share its `window`, its `document.documentElement` and its web storage. Your globals,
timers and listeners live as long as the panel stays loaded.

**Storage is yours — and disposable.** The host gives plugins no storage API and no file write. Keep
your own data: `localStorage` is available and is the intended place for small state (progress ticks,
the last selected tab). Prefix every key with your id (`course-mvp:progress:v1`) — the host uses that
prefix, it is the same storage the app uses, and namespacing is what keeps two plugins apart.

Three consequences, in increasing order of how much they hurt:

1. The origin differs between dev (`http://localhost:<dev-port>`) and production
   (`file://…/dist/index.html`), so data written in one is not visible in the other.
2. **Disabling does not delete data.** A disabled plugin keeps everything it wrote; re-enabling
   resumes where it left off. (The index entry, version and hash stay too.)
3. **Uninstalling DOES delete every key prefixed `<id>:`.** Treat plugin storage as **disposable**:
   never store anything that must survive uninstall (user-authored documents, credentials, anything
   the user would call "my work"). The host has **no write command** — there is no plugin API that
   saves a file or a workspace, so "keep it somewhere safe" is not something the host can do for you:
   if a value must outlive the plugin, it does not belong in `localStorage` — let the user export or
   copy it, and own the saving in your own UI (say so on screen, do not claim the host persists it).
   There is also a "Clear data" button on the settings page (same effect, without uninstalling), and
   the host sweeps the prefix on the next start if the plugin vanished while the app was not running.

If `localStorage` throws (quota, disabled storage), fall back to in-memory state and say so in the UI
instead of crashing the panel.

**Theme is live.** `host.theme()` returns `{ mode, tokens }` for the theme *right now*. It is not a
constant: call it **inside render**, not once in `apply` and stored — a cached palette keeps the old
colours after the user switches light/dark. The tokens are exactly `bg, fg, border, accent, ok,
warn, err, info`; there is no `muted` and no font token. Derive anything else from these (`fg` at
reduced opacity) or use `currentColor`.

**Icons.** Two different things share the name. `surface.json.icon` is a path inside your folder:
the host loads it through its own protocol and uses it as the provider's fallback icon (the tab
strip), unless your provider declares `icon`. `entries[].icon` is also a path, used by the host in
the menu. `provider.icon` is a function `(tab?) => ReactNode` rendered at ~12 px — return an element
from it, not a path.

**Lifecycle.** The host calls `apply` once per load. On reload (a tracked file changed in a `dev`
install, plugin re-enabled, app restart) it first unregisters your kind, disposes the subscriptions
you registered through `host.onEvent`, then imports the file again and calls `apply` again.
A `dev` install (the default for `surface_install`) symlinks the installed id to your folder and
watches the files the panel can load — `panel.js`, `surface.json`, `icon.svg`, and imported
js/mjs/css/json/svg/png/jpg/gif/webp/html/md/txt. Saving one reloads the panel in ~250 ms; no
reinstall and no app restart. `node_modules`, `.git` and dot-directories are skipped, so a big
repo next to the plugin does not trigger reloads. `dev:false` installs a frozen copy instead.

So do all `surfaces.register`
calls **inside `apply`**, never at module top level (a top-level registration runs before the old
provider is gone and leaves a stale one), and keep `apply` idempotent. `apply`'s **return value is
ignored** — the host does not use it as a teardown. For everything the host does not track
(`setInterval`, `window.addEventListener`, an AbortController you own), clean it up yourself in a
component effect; do not leave it running after the panel unmounts.

## Forbidden

- No iframe / no `mock HTML preview` — the panel renders in the app's own React tree.
- No touching `document.body`, no replacing the app root, no global CSS, no reading another
  panel's DOM. Your island is your provider's subtree.
- No `capabilities: ['write']` — plugins cannot ask for write access (the registry rejects it).
- Do not fabricate data. If a command returns `{ ok: false }`, render the reason.

## Tool descriptions stay short

Every tool description in this product is a permanent cost (it rides in the schema of every turn).
The installer's hard cap is **300 characters**; the product budget for a new tool is **≤ 120
characters** and **≤ 3 parameters**. Treat 120 as the line to design to, not 300. Put the tutorial
in this skill — not in the schema. Same rule for the receipt strings you show the user: one line,
actionable.
