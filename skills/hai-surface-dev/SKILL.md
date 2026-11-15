---
name: hai-surface-dev
version: 1.0.0
description: 'Build or debug a panel in the desktop right sidebar - a dashboard, a tool page, or a UI the user wants added. Installs a local ESM plugin and verifies it. Triggers: 插件, 面板, 看板, 右栏, right panel, surface, dashboard, add a tab, 装个面板.'
requires:
  tools: [ui_inspect, surface_install]
---

# Surface plugin development

You are adding a **panel** to the desktop app's right sidebar. It is a plugin: a folder on disk
with `surface.json` + `panel.js` (+ optional `icon.svg`), installed by `surface_install`, loaded by
the renderer through `hai-surface://<id>/panel.js` and registered into the same registry the four
built-in panels (activity / git / file / web) use. Nothing is special-cased: a plugin is a normal
surface provider.

## Before you write code

1. `ui_inspect` with `what:"surfaces"` — what is already registered, and is anything `disabled` or
   carrying an `error`? A failed plugin shows up there with its reason; fix that before adding more.
   `what:"theme"` / `what:"layout"` tell you how the panel will look where it lands.
2. If the user's goal is ambiguous (a chart? a table? a button that does something?), ask once.
   A panel that reads the wrong thing is worse than one question.

## Where things go

- New ability → `group: "plugin"`. **Never** replace or re-register a built-in kind
  (`activity` / `git` / `file` / `web`) — those are the app's own panels.
- `kind` must be exactly `plugin:<id>`, and `<id>` must match `^[a-z][a-z0-9-]{1,40}$`.
  A mismatch is rejected at load time (and the panel never appears).

## The flow

1. Copy `templates/surface/` (in this repo) to the user's workspace, e.g.
   `cp -R templates/surface ./my-board`, then edit `surface.json` (`id`, `kind`, `version`,
   `commands`, `data`) and `panel.js`.
2. `surface_install({ target: "<absolute path to that folder>" })`. It returns a receipt:
   `applied` / `already_installed` / `cleaned` / `rejected` / `error` — **only `applied` means the
   panel is live**. `rejected` carries the reason; fix the folder, install again.
3. `ui_inspect` again: the kind must be in `surfaces` with `disabled:false`. If it has `error`,
   that string is your bug report.
4. `surface_list` to confirm what is installed; `surface_remove({ id })` to take it away
   (the panel's tab becomes a tombstone, nothing else is deleted).

> `surface_install` / `surface_remove` / `surface_list` need `settings.json` to have
> `"features": { "surfaceTools": true }` (default **false** — the tools are absent otherwise).
> `ui_inspect` is always available. If the three surface tools are missing, say so and point the
> user at that setting instead of inventing a workaround.

## panel.js rules (the hard ones)

- **No JSX, no bundler.** `panel.js` is the file the browser imports; there is no compile step.
  Use `React.createElement` (aliased `h`). Anything you would `import` from npm will fail — React
  arrives as an argument (`apply({ host, React, surfaces })`).
- **Export exactly one thing**: `export const surface = { kind, apply({ host, React, surfaces }) }`.
- Register **your own kind only**, once, inside `apply`:
  `surfaces.register({ kind: 'plugin:x', group: 'plugin', title: () => '…', render: () => h(Panel) })`.
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
  | `tool.result` | `{call_id, ok, result?, error?}` | `{ok}` — only for plugin tools (P2) |
  | `composer.quote` | `{text, source?}` | `{ok}` — inserts into the current draft, **never sends** |
  | `ui.open_tab` | `{kind, params?}` | `{ok}` — `kind` must be your own |
  | `ui.toast` | `{message}` | `{ok}` |

  Replies are **values, never exceptions**: undeclared name → `{ok:false, reason:"not_declared"}`,
  refused → `{ok:false, reason:"denied"}`, and a bad quote payload → `invalid_input` / `no_session`.
  Check `ok` and render the reason.
- **The four old read-only commands are disabled** (D11): `workspace_list`, `session_list`,
  `git_snapshot`, `ui_inspect`. They can read real project/session metadata, which a panel must not
  touch. Declaring them in `commands` does nothing — they answer `denied`. Copy the current
  `templates/surface/`, do not copy old examples.
- Subscribe only to event types declared in `surface.json.data` — `host.onEvent` **throws** for an
  undeclared type (so a typo fails at `apply` time instead of silently never firing). Always keep
  the disposer and call it on unmount:

  ```js
  useEffect(() => {
    const off = host.onEvent('agent.delta', (ev) => setText((t) => t + ev.data.text))
    return off
  }, [])
  ```

  | event | data | notes |
  |---|---|---|
  | `agent.delta` | `{text}` | incremental text (host batches ~50 ms) |
  | `agent.tool` | `{name, status, ok?}` | tool name + status only |
  | `agent.usage` | `{input_tokens, output_tokens}` | once, near the end |
  | `agent.done` | `{text}` | terminal |
  | `agent.error` | `{reason, message?}` | terminal |
  | `surfaces_changed` | `{}` | plugin installed/enabled/removed (also spelled `surfaces.changed`) |

  `surfaces_changed` needs no token and works even when the agent channel is unavailable; every
  other event only ever carries **your own** runs (the loader filters by plugin id).
- Theme from `host.theme()` (`{ mode, tokens: { bg, fg, border, accent, ok, warn, err, info } }`).
  Hardcoded colours break one of the two themes.
- Opening another tab is `host.command('ui.open_tab', { kind: 'plugin:<your-id>', params })` —
  your own kind only. A one-line message is `host.command('ui.toast', { message })`. Both are
  conveniences: a panel that only renders is a fine panel.
- Quote into the draft with `host.command('composer.quote', { text, source })`: the text lands in
  the user's composer, `source` becomes a deletable chip on it, and **nothing is sent** (the user
  presses Enter). `text` is capped at 8 KiB; empty/oversized text is `invalid_input`, no active
  session is `no_session`.

## Multiple entrances (`entries`)

One plugin stays **one kind**. If the user needs several pages (overview / schedule / resources),
declare them in `surface.json` — the host draws the menu itself, you do not inject UI:

```json
"entries": [
  { "id": "overview",  "title": "总览", "order": 10 },
  { "id": "schedule",  "title": "课表", "icon": "calendar.svg", "order": 20 },
  { "id": "resources", "title": "资源", "order": 30 }
]
```

Rules the installer enforces (any violation = the whole install is `rejected`, nothing is written):
`entries` ≤ 20, `id` matches `^[a-z][a-z0-9-]{1,40}$` and is unique, `title` non-empty and ≤ 64
characters, `icon` is a path **inside** the plugin folder (a missing icon file is fine — the host
falls back to the kind icon). `title` is either a plain string (rendered in every language) or a
`{ "zh": "课表", "en": "Schedule" }` object — at least one language must be non-empty, each ≤ 64
characters, and the host picks by the UI language at render time.

How the host renders them (three places, all host-owned):

1. right panel `＋` → group **面板插件**;
2. the right panel's empty state (what a new user sees first);
3. settings → 面板插件, one **打开** button per row plus the hint "入口在右栏 ＋ → 面板插件".

Clicking an entrance calls `openTab({ kind: 'plugin:<id>', params: { entry: '<entry id>' } })`:
each entry is its own tab, and the tab title for a **declared** entry comes from `manifest.title`
(so the menu and the tab bar always agree). Read it back through `tab.params.entry`:

```js
function Page({ tab }) {
  const entry = tab.params && tab.params.entry
  return entry === 'schedule' ? h(Timetable) : h(Overview)
}
surfaces.register({ kind: 'plugin:hello', group: 'plugin', title: (tab) => titleOf(tab), render: ({ tab }) => h(Page, { tab }) })
```

- **Zero `entries`** is legal and must still be reachable: the host synthesises one default
  entrance titled from `name` (else the id) and opens it with `params.entry === 'default'`.
  Never ship a plugin that "loads fine but shows nothing anywhere" — that is the one state the
  entrance contract forbids.
- **Routing inside a page** (list → detail → form) is yours; do not open a new entry id per row.
  If a row genuinely needs **its own tab**, put the distinguishing value in `params` (for example
  `params: { entry: 'resources', fileId: 42 }`): the host's default tab identity is
  `entry + the rest of params` (key-order insensitive), so different values = different tabs and the
  same values = the same tab. Declare `dedupeKey: (tab) => String(tab.params?.courseId)` on your
  provider when a *coarser* key is right (e.g. one tab per course, ignoring `fileId`); it must be a
  pure function of `params`.
- **The tab title of a declared entry is host-owned**: it comes from `entries[].title` so the menu
  and the tab bar agree. `title()` on the provider is what the host uses for the **default** entry
  (zero `entries`) and for tabs your plugin opens itself (`ui.open_tab`) — do not fight it for
  declared entries.
- Disabling or uninstalling the plugin removes all entrances immediately; tabs already open turn
  into tombstones (the host does that — do not try to close them from the plugin).

## Host runtime contract (storage / theme / icons / lifecycle)

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

1. The origin differs between dev (`http://localhost:5173`) and production (`file://…/dist/index.html`),
   so data written in one is not visible in the other.
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

**Lifecycle.** The host calls `apply` once per load. On reload (file changed, plugin re-enabled,
app restart) it first unregisters your kind, then imports the file again and calls `apply` again. So
do all `surfaces.register` calls **inside `apply`**, never at module top level (a top-level
registration runs before the old provider is gone and leaves a stale one), and keep `apply`
idempotent. Anything you start must be stoppable: return a cleanup from `useEffect`, and do not
leave a `setInterval` running after your panel unmounts.

## Forbidden (they are not style preferences)

- No iframe / no `mock HTML preview` — the panel renders in the app's own React tree.
- No touching `document.body`, no replacing the app root, no global CSS, no reading another
  panel's DOM. Your island is your provider's subtree.
- No `capabilities: ['write']` — plugins cannot ask for write access (the registry rejects it).
- Do not fabricate data. If a command returns `{ ok: false }`, render the reason.

## Minimum working example

```js
export const surface = {
  kind: 'plugin:hello',
  apply({ host, React, surfaces }) {
    const h = React.createElement
    surfaces.register({
      kind: 'plugin:hello',
      group: 'plugin',
      title: () => 'Hello',
      render: () => h('div', { style: { padding: 12, color: host.theme().tokens.fg } }, 'hi'),
    })
  },
}
```

with `surface.json`: `{"id":"hello","kind":"plugin:hello","version":"1.0.0","entry":"panel.js","commands":[],"data":[]}`.

A panel that talks to the Agent and quotes into the draft (the shape `templates/surface` ships):

```json
{
  "commands": ["agent.session", "agent.run", "agent.cancel", "composer.quote", "ui.toast"],
  "data": ["agent.delta", "agent.done", "agent.error", "surfaces_changed"]
}
```

```js
apply({ host, React }) {
  const h = React.createElement
  let sessionId = ''
  async function ask(input) {
    if (!sessionId) {
      const s = await host.command('agent.session', { title: 'my panel' })
      if (!s.ok) return s.reason
      sessionId = s.session_id
    }
    const r = await host.command('agent.run', { session_id: sessionId, input })
    return r.ok ? r.run_id : r.reason
  }
  host.onEvent('agent.delta', (ev) => render(ev.data.text)) // plus agent.done / agent.error
}
```

## Tool descriptions stay short

Every tool description in this product is a permanent cost (it rides in the schema of every turn).
Keep a new one **≤ 120 characters**, ≤ 3 parameters, and put the tutorial in this skill — not in
the schema. Same rule for the receipt strings you show the user: one line, actionable.

## Self-check (run before you say it works)

Two commands, both take **the directory you just wrote** (absolute path is safest, because
`go test` runs with the package as its working directory):

```bash
go test ./desktop/bridge -run TestSurfaceInstallExternal -count=1 -v -args -dir=<plugin-dir>

cd desktop/app && node e2e/verify_external_surface.mjs --dir=<plugin-dir>
# …or via npm (SURFACE_DIR defaults to the in-repo template):
cd desktop/app && SURFACE_DIR=<plugin-dir> npm run test:surface-install
cd desktop/app && SURFACE_DIR=<plugin-dir> npm run e2e:external-surface
```

What they prove — and each one is a failure, not a warning:

- **Installer** (`TestSurfaceInstallExternal`): the real installer copies your directory into a
  throw-away `HOME` and prints the receipt. `status` must be `applied`, the receipt carries
  `entries: <n>` (how many entrances you declared) and `entryHint` (where the user finds them:
  right panel ＋ → Panel plugins), `index.json` must have exactly one item whose `hash` is
  `sha256(<your entry file>)`, and the installed copy keeps `0600` files / `0700` directories.
  A rejected receipt prints the reason the installer refused (`reason`) — fix the manifest and
  run it again.
- **Browser** (`verify_external_surface.mjs`): loads **your real `panel.js` bytes** into the app
  and clicks what a user would click — every `entries[].id` appears in the ＋ menu with your
  title, the first one renders non-empty content (a crash card or a tombstone fails), the tab
  title equals your declared title, `pageerror` stays 0, and the ＋ menu itself is keyboard
  operable (↑↓ move, Enter opens, Esc closes and returns focus). It also re-checks after a reload.

If you can spare a minute more, one command does the same with **no substitutes** — real
installer → real Electron (temporary `HAI_DATA_DIR`) → real ＋ menu → real `hai-surface://`
panel load (it builds `dist/` + `dist-electron/` on first run):

```bash
cd desktop/app && node e2e/verify_external_surface_electron.mjs --dir=<plugin-dir>
```

Exit codes: `0` pass, `1` your plugin is wrong, `2` the environment is wrong (port taken, or
`--dir` has no `surface.json` / no `panel.js`) — `2` never means "your panel is fine".

`entries[].title` accepts either form; `{ zh, en }` is picked by the host's current language, the
plain string is used for every language:

```json
{ "entries": [ { "id": "overview", "title": "总览" },
               { "id": "schedule", "title": { "zh": "课表", "en": "Schedule" } } ] }
```

Limits of the browser check (do not read a green run as more than it is): it feeds your file as a
single Blob module, so a panel that `import`s other files will fail there (production serves your
whole directory over `hai-surface://`); entry icons are not asserted.
