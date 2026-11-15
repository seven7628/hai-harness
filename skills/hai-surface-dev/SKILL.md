---
name: hai-surface-dev
version: 1.1.1
description: 'Builds or debugs a panel in the desktop right sidebar — dashboard, tool page, PDF/document viewer, or any UI the user wants added. Installs a local ESM plugin and verifies it. Use for 插件/面板/看板/右栏/PDF 阅读器/文档面板/right panel/plugin/surface.'
requires:
  tools: [ui_inspect, surface_install]
---

# Surface plugin development

You are adding a **panel** to the desktop app's right sidebar. It is a plugin: a folder on disk
with `surface.json` + `panel.js` (+ optional `icon.svg`), installed by `surface_install`, loaded by
the renderer through `hai-surface://<id>/panel.js` and registered into the same registry the four
built-in panels (activity / git / file / web) use. Nothing is special-cased: a plugin is a normal
surface provider.

The contract is split into small references. Read only the ones the task needs:

| When | Read |
|---|---|
| Writing `surface.json` / `panel.js`, host API, theme, lifecycle | [`references/panel-api.md`](references/panel-api.md) |
| The panel exposes model-callable tools (P2, approvals, `requires_panel`) | [`references/tools.md`](references/tools.md) |
| The plugin declares several pages (`entries`) | [`references/entries.md`](references/entries.md) |
| Copying a working skeleton or the agent/quote pattern | [`references/examples.md`](references/examples.md) |
| Before claiming it works | [`references/verification.md`](references/verification.md) |

## Before you write code

1. `ui_inspect` with `what:"surfaces"` — what is already registered, and is anything `disabled` or
   carrying an `error`? A failed plugin shows up there with its reason; fix that before adding more.
   `what:"theme"` / `what:"layout"` tell you how the panel will look where it lands.
2. If the user's goal is ambiguous (a chart? a table? a button that does something?), ask once.
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
  an argument). A bare npm `import` will fail in the browser.

## The flow

1. Copy the packaged skeleton: `cp -R <this-skill>/assets ./my-board` (or copy
   `templates/surface/` from the repo). Edit `surface.json` (`id`, `name`, `version`, `kind`,
   `commands`, `data`, and `entries`/`tools` if used) and `panel.js`; the field table is in
   [`references/panel-api.md#surfacejson-fields`](references/panel-api.md#surfacejson-fields).
2. `surface_install({ target: "<absolute path to that folder>" })`. It returns a receipt:
   `applied` / `already_installed` / `cleaned` / `rejected` / `error` — **only `applied` means the
   panel is live**. `rejected` carries the reason; fix the folder, install again. If the tool is
   unavailable, `bash <this-skill>/scripts/install-smoke.sh <plugin-dir>` proves the installer
   accepts it; say clearly that the live in-app install still needs the settings switch.
3. `ui_inspect` again: the kind must be in `surfaces` with `disabled:false`. If it has `error`,
   that string is your bug report.
4. `surface_list` to confirm what is installed; `surface_remove({ id })` to take it away
   (the panel's tab becomes a tombstone, nothing else is deleted).

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

- Export exactly one `surface = { kind, apply({ host, React, surfaces }) }`; register your own
  kind once **inside** `apply`, never at module top level.
- The whole host API is `host.command(name, payload)` + `host.onEvent(type, fn)`. Declare every
  command in `surface.json.commands` and every event in `data`. The host tracks `onEvent`
  subscriptions and disposes them on unload/reload; `off()` is for early removal. `apply`'s return
  value is ignored, so clean up your own timers/listeners.
- Never replace a built-in kind; never touch `document.body` / global CSS / another panel's DOM.
- Read-only is the default. A write-side tool (`read_only:false`) always needs explicit approval;
  the plugin cannot approve itself.
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

See [`references/examples.md`](references/examples.md) for the agent/quote pattern and
[`references/panel-api.md`](references/panel-api.md) for the full host surface.

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
