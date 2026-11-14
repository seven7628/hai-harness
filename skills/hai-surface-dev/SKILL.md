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
- Read data only through `host.command(name, payload)` — and declare the names in
  `surface.json.commands`. The whitelist is authoritative in the host: allowed today are
  `workspace_list`, `session_list`, `git_snapshot`, `ui_inspect`. Anything else returns
  `{ ok: false, reason: "denied" }` **and is audited**. It never throws — check `ok`.
- Subscribe only to ops declared in `surface.json.data` (`host.events.on` throws otherwise).
  Today the useful one is `surfaces_changed`.
- Theme from `host.theme()` (`{ mode, tokens: { bg, fg, border, accent, ok, warn, err, info } }`).
  Hardcoded colours break one of the two themes.
- `host.openTab(spec)` opens another tab; `host.toast(msg)` is a one-line message. Both are
  optional conveniences — a panel that only renders is a fine panel.

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
falls back to the kind icon).

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
  (zero `entries`) and for tabs your plugin opens itself (`host.openTab`) — do not fight it for
  declared entries.
- Disabling or uninstalling the plugin removes all entrances immediately; tabs already open turn
  into tombstones (the host does that — do not try to close them from the plugin).

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

## Tool descriptions stay short

Every tool description in this product is a permanent cost (it rides in the schema of every turn).
Keep a new one **≤ 120 characters**, ≤ 3 parameters, and put the tutorial in this skill — not in
the schema. Same rule for the receipt strings you show the user: one line, actionable.
