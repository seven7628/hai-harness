# Multiple entrances

Part of `hai-surface-dev`. Read this only when the plugin declares several pages.

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
each entry is its own tab, and the tab title for a **declared** entry comes from
`entries[].title` (so the menu and the tab bar always agree). Read the entry id back through
`tab.params.entry`:

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
