# Viberoxy — local context

**Snapshot:** `viberoxy/` @ `c01a7a8` (`origin/develop`, quality-gating line)
**Module:** `viberoxy` (no external deps) · Go 1.21 · 81 Go files, ~30k LOC
**Role:** the data plane. Owns the WAN pool, selection, CONNECT/SOCKS front-ends,
health/canaries, metrics, and the xray child processes.

## Mental model

Viberoxy is a **long-lived proxy with a background subscription loop**. It is not a
library: everything lives in `package main` with package-level globals.

```
subscription URL ──fetch──> parse ──speed/stability test──> promote to WAN slot
                                                              │
client ──CONNECT/SOCKS──> Select(path) ──> xray child per slot ──> upstream
```

## Entry and wiring

| What | Where |
|---|---|
| `main()` | `viberoxy/main.go:1180` — parse env, signal context, `startup` |
| `startup(cfg, ctx)` | `main.go:195` — pool, candidate pool, port allocators, lazy `startServices`, fetch/test/promote loop |
| `runLoop` | `main.go:503` — `FETCH_INTERVAL` ticker, manual `triggerCycle`, `runCycle` |
| `runCycle` | `main.go:1031` — reap drains, health check, fetch, test, write `sorted.txt`, update candidates, rotate |
| `canaryLoop` / `runCanaries` | `main.go:540` / `main.go:588` — probe every active path |
| `startServices` closure | `main.go:251` — starts proxy, SOCKS, metrics, API, canaries once |

### Globals that block reuse

- `triggerCycle = make(chan struct{}, 1)` — manual cycle trigger.
- `candidatePool *cands.Pool` — replacement candidates.
- `subState` + `subStateMu` — last fetch validators/configs.
- `cycleTimingMu`, `lastCycle`, `nextCycle` — control API timing.

These must become fields on a `ProxyService` before the package can be imported or
hot-reloaded.

## Package map

| Path | Responsibility |
|---|---|
| `main.go` | startup, cycle, canaries, rotation, subscription glue |
| `wan.go` | `WANPool`, slot state machine, `Select`, drain/un-drain, `Swap`/`DropAndReplace` |
| `proxy.go` | HTTPS CONNECT front-end |
| `socks.go` | SOCKS5 front-end (RFC 1929 auth, TCP CONNECT only) |
| `relay.go` | splice, TCP tuning, access log, outcome classification |
| `tester.go` | `TestSpeedWithStability`, `DownloadMeasurer` |
| `xray.go` | thin wrappers over `internal/xraycfg` + `internal/xrayproc` |
| `api.go` | control API: WAN slots, candidates, drop, cycle trigger |
| `health.go` | `/metrics`, `/healthz`, `/readyz` |
| `metrics.go` | hand-rolled Prometheus registry, `viberoxy_*` series |
| `internal/proxycfg` | `Config`, env parser, share-link parser, split router |
| `internal/path` | one `Path` per xray lifetime; inflight + generation identity |
| `internal/sched` | P2C + EWMA cost selection, atomic select+reserve |
| `internal/health` | outcome classification, ejection window, canary state |
| `internal/affinity` | HRW (client, site) stickiness |
| `internal/retry` | dial-stage-only bounded retry |
| `internal/cands` | replacement candidate pool (dedupe by Raw, TTL) |
| `internal/auth` | bind policy, proxy users, bearer token, SSRF guard |
| `internal/subs` | hardened fetch: 8 MiB cap, https policy, ETag/304 |
| `internal/ports` | test-port and spare-WAN-port allocators |
| `internal/xraycfg` | xray JSON renderer + golden tests |
| `internal/xrayproc` | xray child lifecycle, Wait-based liveness, reaping |
| `internal/relayio` | bidirectional splice with half-close/timeouts |
| `internal/testutil` | `FakeWAN` harness |

## Listeners

| Listener | Default | Routes |
|---|---|---|
| CONNECT proxy | `127.0.0.1:1080` | CONNECT (405 otherwise) |
| SOCKS5 | off (`SOCKS_PORT=0`) | SOCKS5 CONNECT |
| Metrics/health | off (`METRICS_PORT=0`) | `/metrics`, `/healthz`, `/readyz` |
| Control API | `:1980` (`API_PORT`, read directly in `main.go`) | `/api/viberoxy/wans`, `/wans/{i}/drop`, `/cycle`, `/cycle/trigger`, `/candidates` |

The control API has mutating routes and **no auth by default**; it binds all
interfaces unless `LISTEN_ADDR` is set.

## Config

All via env, parsed centrally in `internal/proxycfg/config.go:ParseConfig` —
**invalid value = hard exit**, unlike Viberayd which clamps/ignores.

`SUBSCRIBER_URL` is required. Notable knobs: `WAN_COUNT` (1–5),
`WAN_BASE_PORT`, `TEST_BASE_PORT`, `MINIMUM_SPEED`, `MAX_TTFB_MS`,
`PROMOTE_PASSES`, `STABILITY_PROBES`, `XRAY_MUX` (default **false**),
`ALLOW_DEGRADED_BOOT`, `ALLOW_DEGRADED_FALLBACK`, `REJECT_DIRECT_EXIT`,
`LISTEN_ADDR`, `PROXY_USERS`, `API_TOKEN`, `DRAIN_MAX`.

`API_PORT` is an exception: read with `os.Getenv` inside `startup`, not part of
`proxycfg.Config`.

## State and outputs

- Candidate pool and WAN pool: **memory only**; no persisted WAN set.
- `sorted.txt`: debug output of per-cycle results.
- Temp xray configs under `os.TempDir()`.
- No `state.json`; a restart begins from an empty pool.

## Coupling to the other services

1. **Viberoxy → Viberayd** over HTTP: `SUBSCRIBER_URL=http://127.0.0.1:8080/sub`.
   Base64 file is parsed again on every fetch. Requires
   `ALLOW_HTTP_SUBSCRIPTION=true`; startup blocks until Viberayd yields configs.
2. **Console → Viberoxy** over HTTP: scrapes `/metrics`, calls
   `/api/viberoxy/*`, probes `/healthz` + `/readyz`.
3. **Console → Viberoxy** via process control: writes `viberoxy.env`, restarts
   the child on apply.

## Risks to remove in the merge

- `package main` + globals prevent importing, embedding, and reloading.
- Config values are captured once by `startServices`, `runLoop`, `canaryLoop`;
  a few helpers re-read the environment per request — no single snapshot.
- Restart-based config changes drop every CONNECT/SOCKS tunnel
  (`ShutdownAll`).
- Self-subscription circular dependency + startup blocking.
- Test range `10800..10823` overlaps Viberayd's `10820..10919` by four ports.
- Control listener and data listener share a bind address but must be separate
  servers; `LISTEN_ADDR` changes require a rebind.
- A panic in cycle/API code can take down the proxy with it.

## Tests

```sh
cd viberoxy && go test ./... -count=1          # ~68 s, green
go test -tags chaos -race -count=1 -run 'TestChaos|TestIntegrity' -timeout 15m .
```

`chaos_test.go` and `integrity_test.go` are the stability regression suite;
keep them when moving code. See `viberoxy/AGENTS.md` for the deep guide.
