# viber-console

Control plane for the [viber stack](https://github.com/amirrezaalavi): bundles
[Viberayd](https://github.com/amirrezaalavi/Viberayd) (subscription aggregator)
and [Viberoxy](https://github.com/amirrezaalavi/Viberoxy) (proxy server), runs
both as supervised children, and provides a single-page WebUI that shows live
status and edits their configuration.

> **Status:** backend + WebUI built. The install/bundle layer (systemd unit,
> install script, compose) is the remaining phase.

## What it does

- **Runs both daemons** — console supervises `viberayd` + `viberoxy` as child
  processes (env-file config, restart from the UI, stderr tail)
- **Aggregates status** — polls Viberayd JSON API + Viberoxy Prometheus metrics
  (stdlib-only parser), serves one `/api/overview` snapshot
- **Edits config** — schema-driven settings form writes the daemons' env files
  atomically (with `.bak` rollback) and restarts them
- **Single-page WebUI** — dark/light theme, mobile-friendly, zero frontend deps
  (vanilla JS + CSS embedded via `go:embed`)

## Quick start

```bash
# 1. Build the console
go build -o build/console ./cmd/console

# 2. Point it at your daemons (or let it spawn them)
#    If VIBERAYD_BIN/VIBEROXY_BIN resolve, the console starts them itself.
VIBER_CONFIG_DIR=/etc/viber \
VIBERAYD_API_URL=http://127.0.0.1:8081 \
VIBEROXY_METRICS_URL=http://127.0.0.1:9090 \
VIBEROXY_API_URL=http://127.0.0.1:1980 \
./build/console

# 3. Open http://127.0.0.1:8090
```

## Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `CONSOLE_LISTEN` | `:8090` | Console HTTP listen address |
| `CONSOLE_POLL_INTERVAL` | `10s` | How often daemons are polled |
| `VIBERAYD_API_URL` | `http://127.0.0.1:8081` | Viberayd management API |
| `VIBERAYD_SUB_URL` | `http://127.0.0.1:8080` | Viberayd subscription endpoint |
| `VIBEROXY_METRICS_URL` | `http://127.0.0.1:9090` | Viberoxy metrics/health |
| `VIBEROXY_API_URL` | `http://127.0.0.1:1980` | Viberoxy WAN control API |
| `VIBER_CONFIG_DIR` | `/etc/viber` | Where the daemons' env files live |
| `VIBERAYD_BIN` / `VIBEROXY_BIN` | `viberayd` / `viberoxy` | Daemon binaries to supervise |
| `CONSOLE_TOKEN` | (empty) | Bearer token; empty = localhost-only |
| `CONSOLE_LOG_LEVEL` | `info` | `info` or `debug` |

## API

| Endpoint | Description |
|---|---|
| `GET /api/health` | Console liveness |
| `GET /api/overview` | One-call dashboard snapshot |
| `GET /api/viberayd/configs?page=&per_page=&state=` | Paginated config table |
| `GET /api/viberayd/urls` | Subscription URL list |
| `GET /api/viberoxy/metrics` | Raw parsed viberoxy metrics |
| `GET /api/viberoxy/wans` | Per-WAN state proxied from viberoxy's control API |
| `GET /api/viberoxy/candidates` | Working replacement candidates |
| `POST /api/viberoxy/wans/{index}/drop` | Drop and replace one WAN |
| `POST /api/viberoxy/cycle/trigger` | Trigger an immediate candidate cycle |
| `GET /api/config/schema` | Editable env-var schema for both daemons |
| `GET/POST /api/config/values` | Read / update config (atomic write + optional restart) |
| `GET /api/processes` | Supervised process status + stderr tail |
| `POST /api/control/restart` | Restart a service (`viberayd`/`viberoxy`/`all`) |

## Structure

```
cmd/console/            # entry point: env parsing, wiring, supervisor boot
internal/collector/     # HTTP client, Prometheus parser, viberayd/viberoxy clients
internal/config/        # env-var schema, atomic env-file store, validation
internal/dashboard/     # poll store, /api handlers, control API, embedded SPA
internal/supervisor/    # child-process manager (start/restart/stop, stderr tail)
docs/                   # data-contract.md + frontend-design.md
scripts/                # smoke.sh (backend), ui-smoke.sh / ui-stack.sh (WebUI)
```

## Development

```bash
go build ./...
go test ./...
go test -race ./...
node --check internal/dashboard/static/app.js
bash scripts/ui-smoke.sh   # boots mock daemons + console, curls SPA + API
```

## License

GPL-3.0 (matches the viber family).
