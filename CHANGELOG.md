# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **CreateMode hot reload** — `surface_install` now defaults to `dev: true`: the installed
  id is a symlink to the source folder and the bridge watches the files the panel loads
  (`panel.js`, `surface.json`, `icon.svg`, imported js/css/images). Saving a file updates
  the content revision and pushes `surfaces_changed`; the panel reloads in ~250 ms with no
  reinstall, restart or re-approval. Pass `dev: false` for a frozen copy.
- **Right panel rail plugin entries** — installed plugins show their own `icon.svg` in the
  collapsed right rail, each with a `⌥⌘1..9` shortcut (also listed in the ＋ menu and on the
  `aria-keyshortcuts` attribute). Clicking one opens that entry in a tab.
- **Right panel maximize** — the panel can cover the session area (left sidebar stays),
  keeping the whole tab strip and every open page; restore with the corner button or `Esc`,
  or close it back to the session.

### Changed

- **TODO moved above the session** — the todo list is no longer pinned at the top of the
  right panel; it renders just above the conversation and still follows the focused
  sub-agent (a sub-agent never falls back to the parent's list). The rail todo badge moved
  with it.
- **Right panel width** — the 平分 / 居中 / 右重 preset buttons are gone. Opening the panel
  is always 右重 (64%); dragging still yields a custom ratio. Legacy `even`/`center` layouts
  migrate to 右重, user-dragged custom ratios are preserved.
- **`hash` is a content revision** — the index hash now covers every file a plugin install
  copies (or, for dev installs, the tracked js/css/json/svg/image files) instead of only the
  entry file.

### Fixed

- Plugin icons stored as a relative path (`icon.svg`) render as an image in the left
  sidebar instead of the literal text `icon.svg`; emoji icons keep rendering as text and a
  broken asset falls back to the kind icon.
- Reinstalling after editing only `surface.json` or `icon.svg` no longer returns
  `already_installed` with the old bytes.
- Walking a plugin directory given through a symlink now works (`filepath.WalkDir` does not
  follow a symlink root), which was also why dev hash/links initially looked empty.

## [0.2.0] - 2026-11-15

### Added

- **CreateMode skill** — the desktop client now ships a bundled `create-mode` skill
  (`desktop/vendor/create-mode-skills/create-mode`) that teaches a model to combine
  the host's exposed panel, agent, command, event, tool and entry capabilities into
  custom extensions. It is packaged via Electron `extraResources` and synced to
  `~/.agents/skills/create-mode` at bridge startup.
- **Read-only plugin session state** — panel plugins can call `agent.snapshot` and
  subscribe to `agent.state` to observe their own session (status, sequence and
  bounded error text; never message content), including runs started by the user
  from the host session list.
- **Plugin tool bridge** — `surface.json` can declare model-callable tools;
  `tool.call` / `tool.result` round-trips, approvals, `requires_panel` gating and
  real-Electron coverage are in place.
- Multi-entry panels (`entries`) and the `composer.quote` / host UI commands are
  available to installed surface plugins.
- A visible **Settings → Surfaces → AI panel creation** switch enables the surface
  tool set (default off for schema cost and fail-closed behavior).

### Changed

- Renamed the previous `hai-surface-dev` skill to **`create-mode`** and rewrote its
  L1 description around custom development: panels, dashboards, viewers, tools and
  workflows built by combining exposed APIs, rather than “make a right-side panel”.
- Moved the skill source into `desktop/vendor/` so it is part of the packaged app,
  not a repository-only document; the bridge syncs it with the same version-aware
  mechanism used by the office and visual skill bundles.
- Updated user-facing settings copy, README and development docs for CreateMode.

### Fixed

- The desktop client no longer reports **“bridge 未运行”** while the bridge is still
  starting. The startup gate now covers the whole not-yet-ready window — including a
  first launch, where the bridge has not been started yet because no workspace has been
  picked: such commands are queued and replayed once the bridge comes up instead of
  failing and pinning a red “bridge 未运行” on the boot screen. A bridge that really
  dies still surfaces the error banner and the restart action.
- Surface Electron E2E now fails fast when the host plugin index is empty, and
  rebuilds stale `dist` / unsets inherited `VITE_DEV_SERVER_URL` before launch,
  instead of silently timing out.
- `agent.state` is isolated from `agent.run` event sequence/run-id namespaces and
  is only emitted when no plugin-initiated run owns the stream.
- Browser/Electron probes clean up their temporary processes and data roots.
- Stopping a group conversation no longer leaves a wake resurrected by an in-flight
  busy-retry: the delivery ledger and the queue now stay in sync, so the stopped view
  reports `queue_len: 0` deterministically.

### Security

- No API keys, `.env` files, key stores or credential material are tracked; the
  repository only contains synthetic redaction fixtures for secret-handling tests.

## [0.1.5] - 2026-10-22

### Added

- OpenDesign aggregator provider (23rd preset) and 11 additional model entries.

### Fixed

- Provider compatibility hardening: output-limit field selection by endpoint
  fingerprint, `finish_reason` normalization, usage alias normalization, upstream
  `Retry-After` / `x-should-retry` handling, and tool-call delta merging by both
  index and id.

Earlier releases are available on the [GitHub releases page](https://github.com/seven7628/hai-harness/releases).

[0.2.0]: https://github.com/seven7628/hai-harness/compare/v0.1.5...v0.2.0
[0.1.5]: https://github.com/seven7628/hai-harness/releases/tag/v0.1.5
