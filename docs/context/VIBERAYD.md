# Viberayd — local context

**Snapshot:** `viberayd/` @ `9b83a59` (`main`, includes `fix/test-quality-verification`)
**Module:** `github.com/amirrezaalavi/Viberayd` · Go 1.21 · 87 Go files, ~14.9k LOC
**Role:** subscription catalog + qualification pipeline. Produces the working set
and serves it as `/sub` and a management API.

## Mental model

```
urls.txt ──fetch──> parse ──dedupe by sha256──> state.json
                                                   │
                              select due ──> TCP prefilter ──> xray test
                                                   │
                                            working.txt + /sub + API
```

It is **already library-shaped** (`internal/daemon`), unlike Viberoxy.

## Entry and wiring

| What | Where |
|---|---|
| `main()` | `viberayd/cmd/viberayd/main.go:13` — flags `-once`, env config, `NewDaemon`, `Run` |
| `Daemon` struct | `internal/daemon/loop.go:20` — `Config`, `State`, `StateMu`, `configs`, `trigger`, `http` |
| `Daemon.Run` | `loop.go:50` — start HTTP, publish output, cycle loop |
| `runCycle` | `loop.go:94` — fetch → merge → select → TCP → xray → apply → output/state |
| `Trigger` | `loop.go:43` — wakes the sleep |
| `shutdown` | `loop.go:305` — stop HTTP, write output, save state |

## Package map

| Path | Responsibility |
|---|---|
| `internal/daemon` | config, state, fetch glue, TCP/xray test orchestration, HTTP, signals |
| `internal/tester` | staged pipeline: TCP → TLS → protocol → xray, domain check, stability probes |
| `internal/models` | `ProxyConfig` tagged union, results, depth |
| `pkg/parser` | per-protocol share-link parsers (vmess/vless/ss/trojan/hysteria2/tuic/wireguard/socks5/reality) |
| `pkg/fetcher` | subscription HTTP fetch |
| `internal/concurrency` | worker pool, port manager, xray subprocess pool |
| `internal/cache` | DNS + result LRU caches |
| `internal/output` | table/json/csv/markdown/checkpoint formatters |
| `internal/orchestrator` | depth/concurrency/timeout heuristic |
| `internal/errors` | sentinel errors + recovery classification |
| `internal/logging` | slog setup |

## Cycle

```
LoadURLs(urls.txt)
  → FetchAndParse (per URL, dedupe sha256)
  → MergeIntoState (new = unknown)
  → SelectCandidates (unknown/failed/unreachable + working due for retest)
  → TCPPing (semaphore 200, cheap prefilter)
  → limitSurvivors(MAX_XRAY_TESTS)
  → XrayTest (bounded parallel 1–20, PortManager + xray child per candidate)
  → ApplyResults (working/failed/unreachable + latency)
  → writeOutputFile(working.txt, latency-ordered) + working.csv
  → SaveState(state.json, atomic)
```

State machine: `unknown → working | failed | unreachable`, and working cycles
through retests (`internal/daemon/state.go`).

## HTTP surface

| Path | Purpose |
|---|---|
| `GET /sub` | base64 of `working.txt` (verbatim share links, latency-ordered) |
| `GET /api/health` | health |
| `GET /api/stats` | counts / cycle stats |
| `GET /api/configs` | paginated config table |
| `GET /api/urls`, `POST /api/urls`, `GET/DELETE /api/urls/{id}` | manage subscription URLs |
| `POST /api/cycle/trigger` | wake the cycle |
| `GET /metrics` | Prometheus text |

Defaults: `HTTP_ENABLED=false`; when enabled, `:8080` for `/sub` and `:8081`
for the API, **on all interfaces, no auth**.

## Config (21 env keys)

`DAEMON_URLS_FILE`, `DAEMON_OUTPUT_FILE`, `DAEMON_STATE_FILE`,
`DAEMON_CYCLE_SLEEP`, `DAEMON_PARALLEL` (1–20), `DAEMON_TIMEOUT`,
`DAEMON_DEPTH`, `DAEMON_KEEP_SUCCESSFUL`, `DAEMON_RETEST_INTERVAL`,
`DAEMON_MAX_LATENCY_MS`, `DAEMON_PORT_BASE`, `DAEMON_STABILITY_PROBES`,
`DAEMON_MAX_XRAY_TESTS`, `DAEMON_DOMAINS`, `DAEMON_DOMAIN_TIMEOUT`,
`DAEMON_DOMAIN_QUORUM`, `DAEMON_DOMAIN_MIN_BYTES`,
`DAEMON_TCP_PING` (UI-only, no daemon reader), `HTTP_ENABLED`, `HTTP_PORT`,
`HTTP_SUB_PATH`, `HTTP_API_PORT`.

`LoadConfigFromEnv` **clamps or ignores bad values** (e.g. `Atoi` error keeps
the default). That is the opposite of Viberoxy's hard-exit policy.

## State and outputs

- `state.json`: versioned map hash → `ConfigEntry` (raw, source, state,
  latency, timestamps, failure count).
- `working.txt`: one verbatim raw URI per line, **ascending latency**.
- `working.csv`: `raw,latency_ms` sidecar.
- In-memory `configs` map: hash → parsed model (not persisted).

## Coupling to the other services

1. **Viberoxy ← Viberayd** via `/sub` (HTTP + base64 + file semantics).
2. **Console → Viberayd** via `/api/stats`, `/api/configs`, `/api/urls`,
   and by writing `urls.txt`.
3. **Console → Viberayd** via process supervision/restart.

## Risks to remove in the merge

- Duplicate parser/model/renderer vs Viberoxy (two grammars, two xray JSONs).
- Duplicate test pipeline vs Viberoxy's speed test: a config can pass one and
  fail the other; `tuic`/`hysteria2` are tested here but cannot be xray WANs.
- `StateMu` is not held while the cycle mutates `State.Configs`
  (`mergeWithParsed`, `ApplyResults`, `writeOutputFile`) — a race with
  `/api/configs` and `/metrics` scrapes.
- `pkg/fetcher` has no size cap / scheme policy (Viberoxy's `internal/subs` does).
- Port range `DAEMON_PORT_BASE` (10820) overlaps Viberoxy's test range.
- File/`/sub` boundary turns metadata changes into wire-format bugs (historical
  `" <latency>ms"` corruption).

## Tests

```sh
cd viberayd && go test ./... -count=1     # ~29 s, green
```

`internal/tester` is the long pole (~29 s). `testdata/` holds subscriptions and
expected working/not-working sets. See `viberayd/AGENTS.md`.
