# viber-console — Frontend + Control Design

> **Status:** Design. Frontend not implemented yet — this is the plan.
> Supersedes the read-only backend scope: viber-console is now the **bundler +
> controller** for both daemons, with a WebUI that edits config and restarts
> services, not just a dashboard.

---

## 1. Goal & hard constraints (from the user)

| Constraint | Meaning |
|---|---|
| Install, bundle, run both projects | viber-console owns the lifecycle of Viberayd + Viberoxy |
| Control both | read status **and** change core parameters of both apps |
| Simple but modern webapp | modern look, not a toy, not enterprise bloat |
| One main page | everything reachable from a single page |
| Mobile-friendly | usable on a phone |
| Server < 150 MB RAM | aggressive lightweight target |
| Few dependencies | keep the family's zero-dep ethos |

---

## 2. Tech stack decision

**Go stdlib backend (existing) + embedded vanilla JS/CSS SPA. Zero new dependencies. No build step.**

| Option | Verdict | Why |
|---|---|---|
| **Vanilla JS SPA + hand-rolled CSS** (chosen) | ✅ | ~0 KB deps, no npm/build toolchain, full control of the modern look, embedded via `go:embed` |
| HTMX + server-rendered HTML | ❌ | one more runtime dep; less "app" feel; we already have a JSON API |
| React/Vue + Vite build | ❌ | build toolchain + node_modules + bundle weight — violates every constraint |
| Tailwind | ❌ | build step or huge CDN dep |
| Pico/Bulma CSS (vendored single file) | 🟡 fallback | acceptable if hand-rolled CSS grows; still one vendored file, no build |

Frontend stack detail:
- `static/index.html` + `static/app.js` + `static/styles.css` — embedded with `//go:embed`
- One fetch-based data layer hitting `/api/*`; polling every 10s (same as backend poll)
- Hand-rolled modern dark theme: CSS custom properties, card grid, status pills,
  system font stack (no webfont download), inline SVG icons (no icon lib)
- No frameworks, no build, no CDN — works offline, tiny, fast

**RAM budget (measured-style estimate):**
| Component | Estimate |
|---|---|
| Go console binary RSS | 15–30 MB |
| Polled snapshots + JSON buffers | < 5 MB |
| Embedded static assets in memory | < 1 MB |
| **Total** | **~20–35 MB ≪ 150 MB** ✅ |

---

## 3. System architecture

```mermaid
flowchart LR
    U[Browser / Phone] -->|HTTPS or localhost| C[console :8090]
    C -->|GET /api/*| VD[Viberayd :8081 JSON API]
    C -->|GET /metrics /healthz /readyz| VXM[Viberoxy :9090 observability]
    C -->|GET/POST /api/viberoxy/*| VXA[Viberoxy :1980 control API]
    C -->|read/write env files + restart| ENV[/etc/viber/viberayd.env, viberoxy.env]
    C -->|spawn/reap| P1[viberayd process]
    C -->|spawn/reap| P2[viberoxy process]
    C -->|go:embed| SPA[static SPA]
```

- **Read path:** console polls both daemons (already built), serves aggregated JSON.
- **Control path (new):** console reads/writes the daemons' **env files** (their only
  config surface) and **restarts the daemon processes**. Atomic write + backup.
- **Process ownership:** console is the **supervisor** — it spawns both daemons as
  children (systemd-free), so it can restart them from the UI and inside Docker.
  (Decision point — see §8.)

---

## 4. Single-page layout (mobile-first wireframe)

```
┌──────────────────────────────────────────────┐
│ ⚡ viber-console      [● ●] [↻]  [≡ menu]    │  header: daemon pills + refresh
├──────────────────────────────────────────────┤
│ 8 working   112 failed   2 WANs   0.08s p50 │  stat cards (4)
├──────────────────────────────────────────────┤
│ Viberoxy — WAN slots                         │
│ ┌─────────┐ ┌─────────┐                     │
│ │ WAN 0   │ │ WAN 1   │   speed, stability, │
│ │ 42.5 Mb │ │ 18.2 Mb │   conns, proto tags │
│ └─────────┘ └─────────┘                     │
├──────────────────────────────────────────────┤
│ Viberayd — configs      [all ▾] [🔍 search] │
│ host │ proto │ state │ latency │ retests    │
│ …                                          │
├──────────────────────────────────────────────┤
│ Settings — core parameters                  │
│ [ Viberayd ▸ ] [ Viberoxy ▸ ]               │
│ SUBSCRIBER_URL      [________________]      │
│ WAN_COUNT           [ 4 ]  (1–5)            │
│ MINIMUM_SPEED       [ 5.0 ]                 │
│ ROUTE_MODE          [proxy-default ▾]       │
│ …                     [Save & Restart]      │
└──────────────────────────────────────────────┘
```

- **One page, four sections**, stacked vertically; sticky header with section nav.
- Mobile: single column, sections collapse, WAN cards stack; `≡` menu jumps to section.
- Desktop (≥900px): stat cards in a row, WAN cards in a grid, config table wider.
- Status pills: `● viberayd up` green / `● down` red; per-WAN pill green/amber/red.

---

## 5. Design language

- **Light/dark theme toggle** (user decision) — default follows `prefers-color-scheme`,
  manual toggle persisted in `localStorage`. CSS variables define both palettes:
  - Dark: `--bg: #0f1115`, `--card: #171a21`, `--accent: #3b82f6`, `--ok: #22c55e`,
    `--warn: #f59e0b`, `--err: #ef4444`, `--text: #e5e7eb`
  - Light: `--bg: #f5f6f8`, `--card: #ffffff`, `--accent: #2563eb`, `--ok: #16a34a`,
    `--warn: #d97706`, `--err: #dc2626`, `--text: #1f2937`
- Cards with 1px border + subtle radius, no heavy shadows; status = color-coded pill.
- System font stack (`system-ui, -apple-system, "Segoe UI", Roboto, sans-serif`) —
  zero font downloads.
- Inline SVG icons (refresh, search, menu, dot, sun/moon) — no icon dependency.
- Numbers formatted with `Intl.NumberFormat`; latency in ms with one decimal.
- Empty/error states: explicit `daemon unreachable` card instead of blank.

---

## 6. API contract additions (backend work)

Existing (read-only, shipped): `/api/health`, `/api/overview`, `/api/viberayd/configs`,
`/api/viberayd/urls`, `/api/viberoxy/metrics`.

New (control):

| Endpoint | Purpose |
|---|---|
| `GET /api/config/schema` | editable env vars for both apps: name, group, type (string/int/bool/enum), default, min/max, description. Drives the settings form |
| `GET /api/config/values` | current values (read from env files + live daemon state) |
| `POST /api/config/values` | update values → validate → atomic write env files (with `.bak`) → **restart affected daemon(s)** |
| `POST /api/control/restart` | restart `viberayd` / `viberoxy` / `console`-managed children |
| `GET /api/processes` | supervisor view: pid, uptime, exit status, restart count, stderr tail (last 50 lines) |

Auth: console binds `127.0.0.1` by default; when bound to a network interface it
requires a bearer token (`CONSOLE_TOKEN`) — checked on every `/api/*` except
`/api/health`.

### Config schema (which params are "core")

**Viberayd group** (`viberayd.env`):
`DAEMON_URLS_FILE, DAEMON_OUTPUT_FILE, DAEMON_STATE_FILE, DAEMON_CYCLE_SLEEP (≥10),
DAEMON_PARALLEL (1–20), DAEMON_TIMEOUT (≥1), DAEMON_DEPTH (enum), DAEMON_KEEP_SUCCESSFUL,
DAEMON_RETEST_INTERVAL, DAEMON_MAX_LATENCY_MS (≥0), HTTP_ENABLED, HTTP_PORT,
HTTP_SUB_PATH, HTTP_API_PORT`

**Viberoxy group** (`viberoxy.env`):
`SUBSCRIBER_URL (required, http/https), FETCH_INTERVAL (≥30), TEST_TIMEOUT (≥3),
DOWNLOAD_SIZE (≥1e6), WAN_COUNT (1–5), WAN_BASE_PORT, TEST_BASE_PORT, PROXY_PORT,
SOCKS_PORT, METRICS_PORT, MINIMUM_SPEED, MAX_TEST_PER_CYCLE, KEEPALIVE_INTERVAL (≥10),
WAN_FAIL_THRESHOLD, STABILITY_PROBES (0–5), ACCESS_LOG, ALLOW_DEGRADED_BOOT, XRAY_MUX,
ROUTE_MODE (enum), DIRECT_DOMAINS, PROXY_DOMAINS, DIRECT_LIST_FILE, PROXY_LIST_FILE`

Schema carries `min/max/enum/required` so the UI validates before submit, and the
backend re-validates before writing (defense in depth). Secret-adjacent fields
(URLs with credentials) are rendered masked and stored 0600.

---

## 7. Config edit + restart flow (the "control" UX)

1. User edits values in Settings form (client-side validation from schema).
2. `POST /api/config/values` → backend validates → writes `viberayd.env.tmp`,
   `chmod 0600`, atomic `rename` (old file → `.bak`), then:
3. If the daemon is a console-managed child → send SIGHUP/restart it; if
   supervisor restarts both, `POST /api/control/restart`.
4. Response: `{saved: true, restarted: [viberoxy], warnings: [...]}`; UI shows a
   toast + re-polls status to confirm the daemon came back.
5. **Rollback path:** if a daemon fails to come up within N seconds after restart,
   the UI offers "restore previous config" (console keeps the `.bak` and restores
   it on explicit request or on boot if the daemon never started).

---

## 8. Bundling & lifecycle (the "install/run" part)

```
viber-console/
├── install.sh            # builds console, installs to /usr/local/bin,
│                         # creates /etc/viber/{viberayd.env,viberoxy.env}, systemd unit (optional)
├── cmd/console/          # supervisor: spawns viberayd + viberoxy as children
├── internal/supervisor/  # process manager: start/stop/restart, stderr tail, restart budget
├── internal/config/      # env-file schema, read/write/validate, .bak rollback
├── internal/dashboard/   # existing aggregation API
└── static/               # SPA (go:embed)
```

**Decision — supervisor model:**

| Model | Pros | Cons |
|---|---|---|
| **Console spawns children (recommended)** | one process tree, restart from UI trivially, works in Docker, no root/systemd dependency | console restart = daemon restart; need stderr capture |
| systemd units | proper service management, auto-restart, journald | console needs root/systemd socket; awkward in Docker; more moving parts |

Recommended: console supervises children, **optional** systemd unit only for the
console itself. Env files are the single source of truth for daemon config; the
console never edits daemon source.

---

## 9. Mobile UX specifics

- Viewport meta, `min-width: 0` grids, touch targets ≥ 44px.
- Sticky header with collapsible `≡` nav (anchor links to the 4 sections).
- Table → card list on mobile: each config becomes a card row (`host`, state pill,
  latency) with a tap-to-expand detail.
- Save/Restart button fixed at bottom on mobile (thumb reach) or inline on desktop.
- Pull-to-refresh optional; manual refresh button always present; auto-poll 10s.

---

## 10. Implementation phases

1. **Supervisor + config backend** (`internal/supervisor`, `internal/config`,
   new API endpoints + tests) — no UI yet.
2. **SPA shell** — index.html + CSS + header/stat cards, dark theme, mobile grid.
3. **Status sections** — WAN slots, configs table/list, daemon-down states.
4. **Settings editor** — schema-driven form, validation, save → restart → toast.
5. **Polish** — process view, rollback button, empty states, accessibility.

Each phase lands as a commit; the UI is verified with the smoke-test mock daemons
plus a headless render check.

## 11. Decisions (user-confirmed)

1. **Supervisor model:** console spawns daemons as children — ✅ confirmed.
2. **Theme:** light/dark toggle (default = system preference) — ✅ confirmed.
3. **Config scope:** §6 lists are the starting point; adjustable during build.
4. **Auth posture:** localhost-only default, token when exposed — assumed OK (flag if not).
5. **UI language:** English (family convention) — assumed OK (flag if not).
