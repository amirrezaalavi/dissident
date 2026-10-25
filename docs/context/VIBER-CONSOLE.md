# Viber Console — local context

**Snapshot:** `viber-console/` @ `a1888b8` (`main`)
**Module:** `github.com/yolka-wiz/viber-console` · Go 1.26 · 20 Go files, ~3.7k LOC
**Role:** supervisor + control plane + embedded SPA. It owns the two daemons'
lifecycle and config files, and renders their state.

## Mental model

```
console :8090
  ├─ poller ──HTTP──> viberayd :8081 /api/stats, /api/configs, /api/urls
  │          └─────> viberoxy :9090 /metrics + /healthz + /readyz
  │                  viberoxy :1980 /api/viberoxy/*
  ├─ supervisor ──exec──> viberayd, viberoxy (restart on crash)
  ├─ config store ──read/write──> /etc/viber/{viberayd,viberoxy}.env
  └─ dashboard /api/* + embedded SPA
```

## Entry and wiring

| What | Where |
|---|---|
| `main()` | `viber-console/cmd/console/main.go:69` |
| `settings` | `main.go:21` — `CONSOLE_LISTEN` (:8090), poll interval (10s), five upstream URLs, config dir, binaries, token |
| HTTP clients | `main.go:78-79` — `collector.NewViberaydClient`, `NewViberoxyClientWithAPI`, 5 s timeout |
| poll store | `internal/dashboard/store.go:40` — `Run` ticker, `poll` holds the write mutex across all upstream calls |
| supervisor | `main.go:90-98` — two children, `AutoRestart=true` |
| routes | `internal/dashboard/handlers.go:24`, `control.go:36` |
| SPA | `internal/dashboard/static.go` — `//go:embed all:static` |

## Package map

| Path | Responsibility |
|---|---|
| `internal/collector` | HTTP clients + Prometheus text parser; upstream payload structs |
| `internal/dashboard` | poll store, JSON handlers, control handlers, embedded SPA |
| `internal/config` | 38-field UI schema + env-file store (`values`, validation, atomic update) |
| `internal/supervisor` | spawn/env/restart/stop children, stderr ring buffer |

## HTTP surface

| Path | Purpose |
|---|---|
| `GET /api/health` | console health |
| `GET /api/overview` | combined snapshot |
| `GET /api/viberayd/configs` | proxied/paginated config table |
| `GET /api/viberayd/urls` | subscription URL list |
| `GET /api/viberoxy/metrics` | parsed metrics |
| `GET /api/viberoxy/wans` | WAN slots |
| `GET /api/viberoxy/candidates` | replacement candidates |
| `POST /api/viberoxy/cycle/trigger` | trigger a cycle |
| `POST/GET /api/viberoxy/wans/{i}/drop` | drop a WAN |
| `GET /api/config/schema`, `/api/config/values` | editable config |
| `GET /api/processes` | child status |
| `POST /api/control/restart` | restart children |
| `GET /`, `/static/*` | SPA |

`CONSOLE_TOKEN` empty means **no auth check**; `CONSOLE_LISTEN` defaults to
`:8090` (all interfaces). `/api/config/values` and `/api/control/restart` are
reachable if the port is reachable.

## Config model

- Two env files: `<VIBER_CONFIG_DIR>/viberayd.env` and `viberoxy.env`.
- Schema: 38 fields (15 viberayd, 23 viberoxy) in
  `internal/config/schema.go`.
- `Store.Values`: schema defaults, then file values win.
- `Validate`: per-key type/range/enum/secret checks.
- `Update`: atomic tmp+rename + `.bak`.
- Apply flow: write env file → `POST /api/control/restart` (or restart service)
  → SIGINT, wait 5 s, spawn again.

## Coupling and failure behaviour

- **Polling, not push:** 10 s interval, 5 s per upstream call; the store mutex is
  held across them, so one slow daemon stalls all UI reads.
- **HTTP contracts are the integration:** every console view is a re-encoding of
  daemon JSON/Prometheus text; shape changes break the UI silently.
- **Supervisor is also the restart mechanism for config changes**, so "Apply"
  and "drop every live tunnel" are the same action.
- **Schema drift is real:** 38 UI fields vs ~60 daemon knobs; 23 daemon knobs
  are not editable; `DAEMON_TCP_PING` is stale; `XRAY_MUX`, `DOWNLOAD_SIZE`,
  `MINIMUM_SPEED` defaults/types disagree.
- Daemons also run standalone; the console starts them but tolerates failure.

## Tests

```sh
cd viber-console && go test ./... -count=1     # ~8 s
```

Tests cover the store, collectors, handlers, config store, and supervisor.
