---
name: create-mode
version: 1.5.0
description: 'Build custom HAI CreateMode extensions by combining the exposed panel, agent, and host APIs — dashboards, document panels, tools, and workflows. Use when a developer wants to create, extend, or debug a custom right panel, plugin, surface, PDF viewer, or integration (插件、面板、右栏、看板、文档面板、PDF 阅读器、自定义开发).'
requires:
  tools: [ui_inspect, surface_install]
---

# CreateMode: build a custom app extension

CreateMode lets you extend the HAI desktop app by combining the capabilities the host exposes:
a panel provider, host commands and events, agent sessions, model-callable plugin tools, and
declarative entries.

**There are two shapes. Pick one before writing anything:**

- **panel (ESM)** — a folder with `surface.json` + `panel.js` (+ optional `icon.svg`). The loader
  imports `panel.js` through `hai-surface://<id>/panel.js`; it registers a provider and renders in
  the host React tree. Best for dashboards/tables/small tools that should look like HAI.
- **web app (URL)** — a folder with only `surface.json` and a `web.url` pointing at your own
  running React/Vite/Next app. No `panel.js`; the host opens it in a native view as a full-panel
  tab, and the page gets `window.haiHost` (the same command/event contract). Best when the UI
  already exists, needs its own router, 3D/WebGL/canvas, or a dev server.

Do not mix the two in one plugin. Read [`references/web-app.md`](references/web-app.md) for the URL
shape; the rest of this file assumes the panel shape unless stated otherwise.

The contract is split into small references. Read only the ones the task needs:

| When | Read |
|---|---|
| Writing `surface.json` / `panel.js`, host API, theme, lifecycle | [`references/panel-api.md`](references/panel-api.md) |
| Building a URL/web-app plugin, `window.haiHost`, dev-server URL, permissions | [`references/web-app.md`](references/web-app.md) |
| The panel exposes model-callable tools (P2, approvals, `requires_panel`) | [`references/tools.md`](references/tools.md) |
| The plugin declares several pages (`entries`) | [`references/entries.md`](references/entries.md) |
| Copying a working skeleton or the agent/quote pattern | [`references/examples.md`](references/examples.md) |
| Before claiming it works | [`references/verification.md`](references/verification.md) |

## Before you write code

1. Decide the shape: **does the UI already exist as a React/Vite/Next app or HTTP service?**
   If yes, use the web-app shape (no `panel.js`, `web.url` + entries). If you are building a small
   host-shaped panel, use the ESM panel shape.
2. `ui_inspect` with `what:"surfaces"` — what is already registered, and is anything `disabled` or
   carrying an `error`? A failed plugin shows up there with its reason; fix that before adding more.
   `what:"theme"` / `what:"layout"` tell you how the panel will look where it lands.
3. If the user's goal is ambiguous (a chart? a table? a button that does something?), ask once.
   A panel that reads the wrong thing is worse than one question.

If `ui_inspect` / `surface_install` are **not in your tool list**, do not pretend to call them:
point at Settings → Surfaces → AI panel creation (or the packaged scripts in “Self-check”) and say
that live host inspection/install was unavailable in this environment. The scripts are the
supported fallback when the surface tools are off.

## Where things go

- New ability → `group: "plugin"`. **Never** replace or re-register a built-in kind
  (`activity` / `git` / `file` / `web`) — those are the app's own panels.
- `kind` must be exactly `plugin:<id>`, and `<id>` must match `^[a-z][a-z0-9-]{1,40}$`.
  A mismatch is rejected at load time (and the panel never appears).
- No JSX and no bundler: `panel.js` is loaded directly, so use `createElement` (React arrives as
  an argument). A bare npm `import` will fail in the browser. **This rule is for the panel shape
  only** — a web-app plugin is a normal bundled app served by your own dev server.
- Web-app shape: `web.url` is the app root (`http`/`https`, no query/fragment/userinfo);
  `entries[].path` is a relative page path (`/`, `/admin`, `#/detail`); no `//evil.com`, no `..`,
  no scheme. The page uses `window.haiHost`, not `host`/`window.desktop`. Optional
  `web.start` lets the host launch the service (MCP-server style: command/args/cwd/env/health);
  without it, you start the service yourself.

## The flow

1. **Panel shape**: copy the packaged skeleton `cp -R <this-skill>/assets ./my-board` (or copy
   `templates/surface/` from the repo). Edit `surface.json` (`id`, `name`, `version`, `kind`,
   `commands`, `data`, and `entries`/`tools` if used) and `panel.js`; the field table is in
   [`references/panel-api.md#surfacejson-fields`](references/panel-api.md#surfacejson-fields).

   **Web-app shape**: copy `<this-skill>/assets/web-surface.json` to `my-app/surface.json`, edit
   `web.url` / `entries[].path` / `commands` / `data`, and do **not** add `panel.js`; start the
   app yourself (`npm run dev`). If the host should start it, copy
   `<this-skill>/assets/web-server.json` **and** `<this-skill>/assets/web-server.mjs` instead,
   edit `web.start` (command/cwd/health/port), and don't run the server manually. Full guide in
   [`references/web-app.md`](references/web-app.md#surfacejson).
2. `surface_install({ target: "<absolute path to that folder>" })`. It returns a receipt:
   `applied` / `already_installed` / `cleaned` / `rejected` / `error` — **only `applied` means the
   panel is live**. `rejected` carries the reason; fix the folder, install again. If the tool is
   unavailable, `bash <this-skill>/scripts/install-smoke.sh <plugin-dir>` proves the installer
   accepts it; say clearly that the live in-app install still needs the settings switch.
3. **Iterate without reinstalling.** `dev` defaults to `true`: the installed id is a symlink to
   your folder and the bridge watches the files it loads (`panel.js`, `surface.json`, `icon.svg`,
   imported js/css/images). Save a file → ~250 ms later the panel reloads with the new bytes; no
   reinstall, no restart, no re-approval. `surface.json` edits (entries/icon/commands) reload too.
   Only the *tracked* files count — `node_modules`, `.git` and dot-dirs are ignored. To freeze a
   stable copy instead, install with `{ dev: false }` (then edits need a reinstall).
4. `ui_inspect` again: the kind must be in `surfaces` with `disabled:false`. If it has `error`,
   that string is your bug report. A reload keeps the tab and the plugin's sessions; a panel is
   re-imported, a web page is reloaded (its in-page state starts over).
5. `surface_list` to confirm what is installed; `surface_remove({ id })` to take it away
   (the panel's tab becomes a tombstone, nothing else is deleted; a dev install only moves the
   symlink, your source folder is untouched).

> `surface_install` / `surface_remove` / `surface_list` need
> `settings.json` → `"features": { "surfaceTools": true }` (default **false** — the tools are
> absent otherwise). The visible switch is **Settings → Surfaces → AI panel creation**.
> `ui_inspect` is always available. If the surface tools are missing, say so and point the user at
> that switch instead of inventing a workaround.
>
> D11 only forbids declaring `workspace_list` / `session_list` / `git_snapshot` / `ui_inspect`
> inside a plugin's `surface.json.commands`. `ui_inspect` is still a **host tool** and step 1
> expects you to call it; the installer rejects it only as a plugin command declaration.

## Hard rules (details in references)

- **Panel shape**: export exactly one `surface = { kind, apply({ host, React, surfaces }) }`;
  register your own kind once **inside** `apply`, never at module top level.
- **Both shapes**: the whole host API is `command(name, payload)` + `onEvent(type, fn)` — via
  `host` in a panel, via `window.haiHost` in a web app. Declare every command in
  `surface.json.commands` and every event in `data`.
- **Panel shape**: the host tracks `onEvent` subscriptions and disposes them on unload/reload;
  `off()` is for early removal. `apply`'s return value is ignored, so clean up your own
  timers/listeners.
- **Web-app shape**: no `panel.js`; one entry = one tab (native view). `window.haiHost.onEvent`
  returns a disposer; the page owns its timers/listeners (they die with the document on reload).
  The page must degrade when
  `window.haiHost` is undefined (opened in a normal browser). Agent runs use the plugin's **own**
  session, not the user's current conversation; use `composer.quote` to put text in the draft.
  Device permissions are denied unless `web.permissions` declares `media`. With `web.start`, the
  host spawns the service on the first tab and stops it on plugin unload/app quit; it gives the
  child a minimal env plus `start.env`, and reuses an already-running `web.url`.
- Never replace a built-in kind; never touch `document.body` / global CSS / another panel's DOM.
- Read-only is the default. A write-side tool (`read_only:false`) always needs explicit approval;
  the plugin cannot approve itself.
- If the panel shows whether its session is running, use `agent.snapshot` (and `agent.state` for
  host-driven changes) — session-level, content-free. `agent.run` events do **not** fire when the
  user drives the plugin session from the left panel; see
  [`references/panel-api.md`](references/panel-api.md).
- Do not fabricate data. Render `{ ok:false, reason }` as the user-visible reason.

## Minimum shape

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

with:

```json
{"id":"hello","name":{"zh":"你好","en":"Hello"},"version":"1.0.0","kind":"plugin:hello","entry":"panel.js","commands":[],"data":[]}
```

`name` is an **object keyed by language**, not a plain string. The full field table (including
`entries`, `tools`, and fields that are deliberately absent) is in
[`references/panel-api.md`](references/panel-api.md#surfacejson-fields).

Web-app shape (no `panel.js`):

```json
{"id":"my-app","name":{"zh":"我的应用","en":"My app"},"version":"0.1.0",
 "kind":"plugin:my-app","web":{"url":"http://127.0.0.1:5173"},
 "commands":["agent.session","agent.run","agent.cancel","composer.quote"],
 "data":["agent.delta","agent.done","agent.error"],
 "entries":[{"id":"home","title":"首页","path":"/"},{"id":"admin","title":"后台","path":"/admin"}]}
```

See [`references/examples.md`](references/examples.md) for the agent/quote pattern,
[`references/panel-api.md`](references/panel-api.md) for the full host surface, and
[`references/web-app.md`](references/web-app.md) for the URL shape.

## Self-check (before you say it works)

Run the packaged validator first, then the real installer; both take the directory you wrote:

```bash
node <this-skill>/scripts/validate-plugin.mjs <plugin-dir>
bash <this-skill>/scripts/install-smoke.sh <plugin-dir>
```

For the browser/Electron checks, exit codes, and what each assertion proves, read
[`references/verification.md`](references/verification.md) — the browser tier is self-contained
(port 5232, no dev server/5173), and an Electron red must be compared with the template control
before you touch the panel.

For a web-app plugin, `validate-plugin.mjs` / `install-smoke.sh` still validate the manifest
(no `panel.js` required when `web` is present). Then check the runtime by hand:
`＋` → 面板插件 → each entry opens its own tab; switching tabs keeps page state; stopping the
dev server shows the host error + retry; opening the same URL in a normal browser leaves
`window.haiHost` undefined.
