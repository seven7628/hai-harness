# Web app surfaces (URL plugins)

Part of `create-mode`. Read this when the extension is an **existing web app** (React / Vite /
Next / any HTTP service), not a host-rendered panel. For the ESM panel shape read
[`panel-api.md`](panel-api.md).

## Two shapes, pick one

| | panel (ESM) | web app (URL) |
|---|---|---|
| Content | `panel.js` renders into the host React tree | a page served by your own HTTP app |
| Needs a server | no | yes (dev server or deployed URL) |
| Host API | `host.command` / `host.onEvent` | `window.haiHost.command` / `.onEvent` |
| Best for | dashboards, tables, small tools that should look like the host | an existing React/Vite/Next UI, 3D/WebGL/canvas, audio/mic, its own router |
| `panel.js` | required | **not used** |

Do not mix the two in one plugin. A plugin is one shape; if it has both an app and a host-shaped
panel, ship two plugins (or move the host-shaped part into the app through `window.haiHost`).

## surface.json

```json
{
  "id": "learning-world",
  "name": { "zh": "学习世界", "en": "Learning world" },
  "version": "1.0.0",
  "kind": "plugin:learning-world",
  "web": { "url": "http://127.0.0.1:5173" },
  "commands": ["agent.session", "agent.run", "agent.cancel", "agent.snapshot", "composer.quote", "ui.toast"],
  "data": ["agent.delta", "agent.done", "agent.error", "surfaces_changed"],
  "entries": [
    { "id": "learn", "title": "学习世界", "path": "/", "order": 10 },
    { "id": "admin", "title": "课程后台", "path": "/admin", "order": 20 }
  ]
}
```

- **No `panel.js`.** `entry` is ignored when `web` is present; you can leave an old `panel.js` in
  the folder while migrating, it will not be loaded.
- `web.url` is the **root** of the app: `http` or `https`, optional path, **no query/fragment/
  username/password**. Set it to your dev server (`http://127.0.0.1:5173`) or deployed origin.
- `web.start` is **optional**. With it, the host starts the service on the first tab (MCP-style:
  configure a command, connect to it); without it, the service is yours to run. Schema:

```json
{
  "web": {
    "url": "http://127.0.0.1:5235",
    "start": {
      "command": "node",
      "args": ["server.mjs"],
      "cwd": ".",
      "env": { "PORT": "5235" },
      "health": "/health",
      "timeout_ms": 20000
    }
  }
}
```

  `cwd` is relative to the plugin folder (or an absolute path when the app lives in another repo);
  relative paths cannot escape via `..`/symlinks. `health` is a path relative to `web.url`; the host
  polls it until an HTTP response `<500`, then loads the page. The host first probes `health` — an
  already-running service is **reused**, not spawned twice. It stops the service when the plugin is
  disabled/uninstalled or the app quits; closing a tab does not stop it. The child gets a minimal
  environment (`PATH`/`HOME`/…) plus `start.env`, never the host process's secrets. Command and
  args are passed to `spawn(..., { shell: false })`; use an absolute executable path if `PATH` on
  the app's launch environment is not what you expect.
  Minimal pair: `<this-skill>/assets/web-server.json` + `web-server.mjs`. Full demo (two pages,
  Agent button, health endpoint): `templates/surface-web-server/` in the repo.
- `entries[].path` is a **relative** page path: `/`, `/admin`, `#/detail?id=1`, `?tab=2` are fine.
  It must not contain a scheme/host (`https://…`, `//evil.com`) or `..` — the installer and the
  index reader both reject it, and the host joins with plain string concatenation, never
  `new URL(path, base)` (which would switch origin on `//evil.com`).
- One entry = **one tab**. Each tab owns a separate native view and keeps its page state when you
  switch tabs. Closing the tab closes the view; the server is not touched.
- Zero `entries` is legal: the host synthesises one default entrance titled from `name` and opens
  `web.url + "/"`.
- `commands` / `data` are the **same whitelist** as an ESM panel. Declare every command you call
  and every event you subscribe to; see the command/event tables in
  [`panel-api.md`](panel-api.md#paneljs-rules-the-hard-ones--esm-panel-shape-only). `not_declared` is returned for an
  undeclared command; an undeclared event throws in `onEvent`.
- `web.permissions` is optional. The only supported value today is `"media"` (microphone/camera;
  a learning app uses it for read-aloud). Omit it and the page gets no device permission.

The installer validates the URL/path and writes the normal index entry. In a copy install the
content hash covers every file; in a dev install it covers the tracked app files (including
`surface.json`). After editing `surface.json`, run `surface_install` again on the same folder: the
new hash unloads and re-registers the plugin. Open tabs then use the new provider; a changed
`web.url` reloads the native view. Editing the app's own code is your dev server's HMR — no
reinstall needed.

## `window.haiHost` (inside the app)

The page is loaded in a sandboxed `WebContentsView` with a preload; the host exposes exactly two
functions:

```ts
const host = window.haiHost
if (!host) {
  // Opened in a normal browser: no host API. Degrade gracefully.
}

const s = await host.command('agent.session', { title: '学习世界' })
const r = await host.command('agent.run', {
  session_id: s.session_id,
  input: '把这节课的重点整理成三道练习题',
})
// r = { ok: true, run_id, session_id } — content arrives as events

const off = host.onEvent('agent.delta', (ev) => append(ev.data.text))
host.onEvent('agent.done', () => finish())
host.onEvent('agent.error', (ev) => fail(ev.data.reason))
```

- `command(name, payload)` returns `{ok, reason?, data?}` and **never throws** (same contract as
  the ESM `host.command`). `agent.*` replies are also spread at the top level (`r.session_id`,
  `r.run_id`) so both `r.session_id` and `r.data.session_id` work. Unknown/undeclared →
  `{ok:false, reason:'not_declared'}`.
- `onEvent(type, fn)` returns a disposer. Undeclared type → **throws synchronously** (a typo should
  fail at startup, not silently never fire). The envelope is the same `{type, seq?, run_id?,
  session_id?, data}` as the ESM host.
- `window.haiHost` is **not** `host`, `window.desktop`, or `window.parent`; those do not exist in the
  page. Do not try to import a host module from npm.
- The API is injected before the page's scripts run. If you use React, read it inside a component
  effect / event handler (not at module top-level), and guard for `undefined` so the same build
  still runs in a normal browser.

### Agent session semantics

`agent.run` uses a **session owned by this plugin**, not the user's current conversation. It shows
up in the left panel under the plugin's group and is isolated from the main chat. That is the
default and the recommended shape (least privilege: a page cannot read the user's conversation).

If you want to put text into the user's **current draft**, call `composer.quote`; it only inserts,
it never sends. Do not expect `agent.run` to append to the current conversation.

If several tabs are open, each view can create its own session. Events are delivered to **all
views of the same plugin**; filter by `ev.session_id` / `ev.run_id` before appending. Events that fired while no view was open are dropped. `agent.snapshot` reports status/seq/quota
(`running` / `idle` / error) for one of your sessions, **not** content. `agent.state` is the same
status fact pushed by the host when the session is driven from the host UI (not by your own
`agent.run`); do not use it to rebuild a transcript. Persist any UI state you need in the app itself.

## Dev loop

1. Start your app on a fixed port: `npm run dev -- --port 5173` (Vite) or the framework's
   equivalent. If you declare `web.start`, the host starts it for you on first open; if not, keep
   it running yourself. The host never starts a service that has no `web.start`.
2. Put a `surface.json` in a tiny folder (no `panel.js` needed) and install it with
   `surface_install({ target: "<absolute folder>" })`.
3. Open an entry from the right panel `＋` → 面板插件. You get a full-panel tab, not a preview card.
   With `web.start`, you should see the host's “starting service” state before the page appears.
4. Edit `surface.json` (URL/entries/commands) → run `surface_install` again on the same folder;
   the new hash makes the loader unregister/re-register the plugin, and open tabs pick up the new
   provider (a changed `web.url` reloads the view). Edit the app's own code → your dev server's
   HMR handles it; the host view is just a browser tab.
5. If the page or the service cannot be reached, the **host** shows a loading/error state with the
   URL, the last service log lines and a retry button. There is no plugin-authored
   “re-detect / fullscreen / preview” chrome.

The page is a top-level document, so HMR WebSockets and normal CORS work as in a browser. It must
be reachable from the machine running HAI (localhost or the LAN URL you declared).

## Security / consent

- The view is sandboxed: no Node, no `window.desktop`, no access to the host DOM, a separate
  persistent partition (`persist:surface-<id>`). It cannot read another plugin's storage.
- `web.start` is an **authorized subprocess**: the command/args/cwd are shown in Settings and the
  child gets only the minimal env. Do not put secrets in `start.env`; it is readable in the
  manifest and masked in the UI. Prefer a fixed `127.0.0.1` port and a real `health` path.
- The manifest's `commands` / `data` are the **only** capabilities. The install itself requires
  approval of the target folder; after install, Settings → 面板 shows the declared commands/data
  (and the web URL / device permissions) for review. Treat the manifest as the permission prompt.
- Navigation is locked to the declared origin. External links open in the system browser;
  `window.open` does not create a host window.
- Device permissions are denied unless `web.permissions` declares `media`. Do not ask for more
  than the app actually uses.
- Do not store API keys or other secrets in the page's `localStorage`; the partition is persistent
  but readable by any script on that origin. Use host commands for agent work, and keep the app's
  own backend credentials server-side.

## Verification checklist

- `surface_install` returns `applied`; `ui_inspect` shows `kind:"plugin:<id>"` with `disabled:false`.
- With `web.start`: before the first open the port has no listener; opening a tab starts the
  service, the page appears, and a second tab **reuses the same process** (not a second server).
- Kill the service process: the host shows the error + last logs; Retry spawns a new process.
  Closing a tab leaves the service running; disabling the plugin / quitting the app stops it.
- `＋` → 面板插件 shows every declared entry; clicking two entries opens **two tabs**.
- Switching tabs preserves each page's in-app state (a counter/route/lesson progress), not a
  reload. Collapsing and reopening the right panel also preserves it.
- Closing a tab closes the view (no orphan page process); reopening starts fresh.
- Stopping the dev server and opening a tab shows the **host** error + retry, not a plugin card.
- Open the same URL in a normal browser: `window.haiHost` is `undefined`; the app still renders.
- If the plugin declares tools: the model sees `plugin__<id>__<name>`; `tool.call` arrives in
  `onEvent`, and your `tool.result` reply settles it. `requires_panel` tools are gated on a tab of
  this plugin being open.
