# Changelog

## v0.1.0 — 2026-10-27

First unified release of the `dissident` monorepo. The repository brings the
single-process dissident service together with the three original trees it was
built from:

- `viberoxy/` — Viberoxy (quality-gating `develop` line, `c01a7a8`)
- `viberayd/` — Viberayd (`9b83a59`)
- `viber-console/` — Viber Console (upstream `main`)

The service itself lives at the repository root and does not call the legacy
modules at runtime; they are kept for provenance and reference.

### Added

- One Go process for the subscription catalog, staged WAN qualification, active
  WAN management, the HTTP CONNECT/SOCKS5 proxy, and the server-rendered panel.
- A 1 Mbps WAN gate measured against Cloudflare's 1 MB download endpoint, with
  a 10 MB fallback.
- A 32 KiB POST upload check and a 128 KiB HTTP/2 body check, so a WAN that can
  download but cannot carry uploads or real HTTP/2 traffic is rejected.
- Active-WAN speed liveness every 5 minutes; a sustained sub-1 Mbps result
  triggers replacement.
- Make-before-break WAN replacement with a 5-minute replacement cooldown and
  duplicate-trigger suppression.
- Parallel qualification with bounded stage workers.
- Bounded source fetching and access logging.
- Reproducible `make release` cross-builds for linux/amd64 and linux/arm64,
  plus a tag-driven GitHub release workflow.

### Changed

- Default qualification profile is now `speed_size = 1000000`,
  `min_speed_mbps = 1`, and `stability_passes = 3`.
- WAN verification now requires upload and HTTP/2 checks in addition to the
  download gate.
- Example and deployment configuration use the Cloudflare 1 MB speed endpoint
  with the OVH 10 MB endpoint as a fallback.

### Fixed

- HTTP/2 streams no longer stall at the default 65,536-byte window before the
  WAN is promoted.
- The previous 15-second liveness/replacement loop is bounded and rate-limited.
- Active WAN recovery and source-fetch/logging growth are bounded.

### Known limitations

- Panel and proxy have no authentication or TLS yet; bind them only on a
  trusted network.
- One active WAN at a time. Ready entries are metadata; only the active WAN
  runs xray.
- Qualification can leave no active WAN when every candidate fails the speed,
  upload, or HTTP/2 gate. Free subscription endpoints remain the limiting
  factor.
- The legacy trees are not wired into the dissident binary; their tests are run
  separately by `make test`.

Licence: GPL-3.0.
