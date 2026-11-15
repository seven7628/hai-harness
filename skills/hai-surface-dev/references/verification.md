# Verify before you say it works

Part of `hai-surface-dev`. Run the packaged scripts first, then the browser check; use Electron only
when the browser check is not enough.

Use the skill-relative path (not `../scripts`, which only works if your cwd is `references/`):

```bash
node <this-skill>/scripts/validate-plugin.mjs <plugin-dir>
bash <this-skill>/scripts/install-smoke.sh <plugin-dir>
```

`validate-plugin.mjs` is a **narrow** static preflight: manifest shape, entry exists, D11 commands,
manifest/tool limits, token names, no JSX/bare imports. It is **not** a second installer and does not
simulate the loader. `install-smoke.sh` runs the real installer in a throw-away HOME and is the
authority for `applied` / `rejected`.

Both checks take **the directory you just wrote** (absolute path is safest, because `go test`
runs with the package as its working directory):

```bash
go test ./desktop/bridge -run TestSurfaceInstallExternal -count=1 -v -args -dir=<plugin-dir>

cd desktop/app && node e2e/verify_external_surface.mjs --dir=<plugin-dir>
# …or via npm (SURFACE_DIR defaults to the in-repo template):
cd desktop/app && SURFACE_DIR=<plugin-dir> npm run test:surface-install
cd desktop/app && SURFACE_DIR=<plugin-dir> npm run e2e:external-surface
```

The browser check starts its **own** Vite instance on hard-coded port **5232** and drives Chromium;
it does **not** need `npm run dev` and must not use 5173. If 5232 is busy, stop the other process or
edit the script's `PORT` — a port conflict is `exit 2`, not a plugin failure.

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

When the host build is healthy, one more command does the same with **no substitutes** — real
installer → real Electron (temporary `HAI_DATA_DIR`) → real ＋ menu → real `hai-surface://`
panel load (it builds `dist/` + `dist-electron/` on first run):

```bash
cd desktop/app && node e2e/verify_external_surface_electron.mjs --dir=<plugin-dir>
```

**Run the control before blaming your panel.** The same command on the repo's own template:

```bash
cd desktop/app && node e2e/verify_external_surface_electron.mjs --dir=<repo>/templates/surface
```

If the control fails at the same step, the **host build/environment** is broken, not your plugin;
mark Electron `BLOCKED`, report the control failure, and rely on the browser tier + installer. Do
not rewrite a panel that already passed browser render to chase a red Electron run.

The script now checks the host's `surface:index` **before** clicking the menu, rebuilds
`dist`/`dist-electron`/`hai-bridge` when their sources are newer, and clears inherited
`VITE_DEV_SERVER_URL`. Run it from the checkout that owns the app you are testing: another
worktree's old `dist` produces an empty index, which used to look like a 30 s menu timeout.
An empty host index is a **precondition failure (`exit 2`)**, not a plugin failure.

Exit codes: `0` pass, `1` plugin **or host** failure (use the control to tell which), `2` the
environment is wrong (port taken, `--dir` has no `surface.json` / no `panel.js`, or the host index
has no enabled plugin) — `2` never means "your panel is fine".

`entries[].title` accepts either form; `{ zh, en }` is picked by the host's current language, the
plain string is used for every language:

```json
{ "entries": [ { "id": "overview", "title": "总览" },
               { "id": "schedule", "title": { "zh": "课表", "en": "Schedule" } } ] }
```

Limits of the browser check (do not read a green run as more than it is): it feeds your file as a
single Blob module, so a panel that `import`s other files will fail there (production serves your
whole directory over `hai-surface://`); entry icons are not asserted.
