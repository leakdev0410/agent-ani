# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Status
- Project archived. No further development planned. Snapshot published as-is.
  See README for the maintenance notice.

## [0.1.1] — 2026-09-22

### Changed
- Public-release cleanup: removed personal information from `internal/persona/ani.txt`
  (the persona is a template now — fill in your own details before using).
- Replaced project-specific reference in `internal/skills/skills.go` description
  with a generic example.
- Expanded `.gitignore` to cover side-project directories (`agent-ani/`,
  `marketing/`, `marketing-directions.html`, `*.mp4`, `node_modules/`)
  that live alongside the repo but are not part of the bot codebase.
- Added "no longer maintained" notice to `README.md`.

## [0.1.0] — 2026-09-14

### Added
- Initial public release.
- Single-owner Telegram bot with SQLite-backed durable memory
  (core state, diary, observations, topics) and auto-recall via tool-calling.
- OpenRouter model integration with mid-session `/model`, `/provider`,
  `/providers`, `/status`, `/restart`, `/help` commands.
- Proactive message scheduler (per-chat plans, idle session detection,
  memory-job wake channel).
- Memory journal with bounded retry and persistent delivery state.
- Optional semantic-search indexer (hybrid FTS5 + embedding recall).
- Persona + skills file loading via `internal/persona` and `internal/skills`.
- systemd unit file and helper scripts for Ubuntu 24.04 deploys.

### Changed
- Restructured repository to standard Go project layout.
  - Entry point moved to `cmd/ani-telegram/main.go` (thin wrapper).
  - Orchestration code moved to `internal/app/`.
  - `.env.example` moved to `configs/.env.example`.
  - Deploy helper scripts moved from `deploy/` to `scripts/`.
  - Added `LICENSE`, `CHANGELOG.md`, `CONTRIBUTING.md`, `docs/ARCHITECTURE.md`.

### Removed
- Personal memory artefacts and old backups removed for public release.
  See git history (or the pre-public snapshot) for the prior layout if needed.