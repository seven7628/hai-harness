# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses
[Semantic Versioning](https://semver.org/).

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

- Surface Electron E2E now fails fast when the host plugin index is empty, and
  rebuilds stale `dist` / unsets inherited `VITE_DEV_SERVER_URL` before launch,
  instead of silently timing out.
- `agent.state` is isolated from `agent.run` event sequence/run-id namespaces and
  is only emitted when no plugin-initiated run owns the stream.
- Browser/Electron probes clean up their temporary processes and data roots.

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
