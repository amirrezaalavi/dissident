# Coupling map — what actually talks to what today

## Runtime topology

```
clients ──CONNECT/SOCKS──> viberoxy :1080/:0
                              ├─ WAN xray slots 127.0.0.1:10700..10703
                              ├─ test xray 127.0.0.1:10800..10823
                              ├─ control API :1980
                              └─ metrics/health :9090 (optional)
                              ▲
                              │ GET http://127.0.0.1:8080/sub  (base64 of working.txt)
                              │
                         viberayd :8080 /sub, :8081 /api
                              ├─ test xray pool 127.0.0.1:10820..10919
                              └─ state.json + working.txt + working.csv

viber-console :8090
   ├─ polls viberayd :8081 and viberoxy :9090/:1980
   ├─ edits /etc/viber/{viberayd,viberoxy}.env
   └─ supervises both binaries (SIGINT → wait 5s → restart)
```

## Boundary inventory

| # | Boundary | Transport / format | Producer → Consumer | What breaks |
|---|---|---|---|---|
| 1 | Working set | HTTP GET `/sub` → base64 → `working.txt` lines | viberayd → viberoxy | both must run; http opt-in; startup blocks; re-parse per fetch |
| 2 | Catalog stats | HTTP JSON `/api/stats` | viberayd → console | shape drift; 5 s timeout; poll delay |
| 3 | Config table | HTTP JSON `/api/configs?page&per_page` | viberayd → console | pagination mismatch; lock contention |
| 4 | URL management | HTTP JSON `/api/urls` (+ file `urls.txt`) | console ↔ viberayd | two writers, no coordination |
| 5 | Proxy metrics | HTTP `/metrics` Prometheus text | viberoxy → console | text parser coupling; metric renames |
| 6 | Proxy control | HTTP JSON `/api/viberoxy/*` | console → viberoxy | no auth; JSON shape drift |
| 7 | Health | HTTP `/healthz`, `/readyz` | viberoxy → console | readiness semantics duplicated |
| 8 | Configuration | `*.env` files on disk | console → both daemons | no schema authority; restart to apply |
| 9 | Lifecycle | `exec` + signals | console → both daemons | console owns restarts; crash loops |
| 10 | Binary presence | `VIBERAYD_BIN` / `VIBEROXY_BIN` | console → OS | version drift between children |

Every one of these becomes a direct Go call or a shared immutable snapshot in the
single-process design.

## Duplicated work

| Concern | Viberoxy | Viberayd | Consequence |
|---|---|---|---|
| Share-link parsing | `internal/proxycfg/parse.go` | `pkg/parser/*` | two grammars, two rejection policies |
| Xray config rendering | `internal/xraycfg/build.go` | `internal/tester/xray_config.go` | a link can pass one, fail the other |
| Subscription fetch | `internal/subs` (8 MiB cap, https-only, ETag) | `pkg/fetcher` (no cap/scheme policy) | weaker path handles arbitrary URLs |
| Testing | speed + TTFB + stability | TCP/TLS/protocol/domain/stability | up to two passes, two xray children per config |
| Port allocation | `internal/ports` (10800 + 20 + 4) | `PortManager(10820, 100)` | **10820–10823 collision** |
| Config | 35 env knobs, hard exit | 21 env knobs, clamp/ignore | two validation philosophies |
| "Working" definition | promoted WAN | `state=working` | UI can show configs that can never be WANs |

## Config drift (evidence)

- Console schema: **38 fields**; daemons read **~60 knobs**.
- **23 daemon knobs missing from the UI**, including `ALLOW_DEGRADED_FALLBACK`,
  `ALLOW_HTTP_SUBSCRIPTION`, `ALLOW_PRIVATE_TARGETS`, `API_PORT`,
  `API_TOKEN`, `DRAIN_MAX`, `MAX_TTFB_MS`, `PROMOTE_PASSES`,
  `PROXY_USERS`, `REJECT_DIRECT_EXIT`, `DAEMON_DOMAINS`,
  `DAEMON_STABILITY_PROBES`, `DAEMON_MAX_XRAY_TESTS`, `DAEMON_PORT_BASE`.
- **Stale UI field:** `DAEMON_TCP_PING` — no daemon reads it.
- **Defaults disagree:** `XRAY_MUX` (UI true / Viberoxy false),
  `DOWNLOAD_SIZE` (10 MB / 1 MB), `MINIMUM_SPEED` (int / float ≥ 0.1).
- **`API_PORT`** is read directly from env in `main.go`, absent from both the
  config struct and the schema.

## Failure amplification

A failure in one process is currently a failure of one *component*:

- Console down → no UI, daemons keep proxying (good).
- Viberayd down → Viberoxy starves on restart (no new `/sub`), no refill.
- Viberoxy down → traffic drops, console shows unreachable.
- Console "Apply" → restarts a daemon → **all live tunnels on that daemon drop**.
- Slow console poll → store mutex held across 5 s network calls → UI stalls.
- Viberayd cycle races its own `/api/configs` reader (`StateMu` not held while
  mutating `State.Configs`).

The merged service must preserve the good property (UI failure cannot stop the
proxy) while removing the bad ones (no self-subscription, no restart-to-reload).

## What disappears in one process

| Today | After |
|---|---|
| `GET /sub` HTTP hop | catalog snapshot passed directly to the WAN manager |
| `working.txt` as transport | export only, for external clients |
| Poll every 10 s | in-process snapshot read on request |
| `*.env` + restart | one config snapshot + reconcile |
| Two parsers / renderers / testers | one of each |
| Two port pools racing | one port map validated at startup/apply |
| Console supervises two binaries | one binary supervises modules |
