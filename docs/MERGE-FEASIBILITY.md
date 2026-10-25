# Feasibility: merging Viberoxy + Viberayd + Viber Console into one service

**Status:** feasibility assessment (no production code changed)
**Date:** 2026-10-24
**Baseline commits:** Viberoxy-fix `c01a7a8`, Viberayd-fix `753a3d7`, viber-console `a1888b8`
**Toolchain:** Go 1.26.6 linux/amd64, xray 26.3.27 available

## 0. Assumptions

- "Single service" means **one deployable artifact and one systemd unit / container**, ideally **one OS process**. `xray-core` remains a child process: it is an external binary, and keeping it out-of-process is a stability feature, not something to merge.
- Target is Linux (systemd or Docker). Windows is not a goal (already untested in both daemons).
- Existing client ports and the `/sub` URL should keep working through a compatibility window.
- The family's **zero external Go dependencies** property is worth preserving.
- All three projects are GPL-3.0, so the licences are compatible.

---

## 1. Verdict

**Yes, this is technically feasible and worth doing — but it is a refactor, not a repository merge.**

There is no fundamental blocker:

- All three are Go, stdlib-only, and build with the same toolchain.
- Their test suites are green today (see `2.3).
- Viberayd is already library-shaped (`internal/daemon`, `internal/tester`, `pkg/parser`).
- Viber Console is the natural shell for the merged service: it already owns the UI, the config editor, and the process lifecycle.
- Viberoxy's data-plane design (path identity, make-before-break swap, drain, canary) is exactly what must survive the merge.

The work is substantial because Viberoxy is **not** library-shaped:

- Its entire service is `package main` with package-level globals (`triggerCycle`, `candidatePool`, `subState`, `rotation`).
- The three projects contain **two parsers, two xray-config renderers, two subscription fetchers, two test pipelines, two port allocators, and two config systems**.
- The console's config schema is already **23 keys behind** the daemons and contains one key that no daemon reads (`3.1).
- The two xray test port ranges **overlap by four ports** at their defaults (`3.4).
- A console "apply config" today restarts a daemon, which drops every live proxy connection (`3.5).

A naive "copy all three into one folder and call three `main()`s" is not possible in Go (one `main` per binary, and `package main` cannot be imported). The realistic path is a staged strangler:

| Milestone | What it delivers | Effort (1 engineer) |
|---|---|---|
| Single binary, three components in-process, behaviour unchanged | One artifact/PID, one shutdown, one log stream | 2–4 weeks |
| Unified config + live reload for the safe 80–90% | No restart for most knobs; one schema/UI | +3–4 weeks |
| True unification (one parser, one renderer, one pipeline, shared catalog) | Removes the double work and divergent semantics | +4–7 weeks |
| Production hardening (rolling WAN changes, soak/chaos, migration) | Meets the stability goals with evidence | +3–4 weeks |

**Recommendation:** do it in phases, and do **not** big-bang it. Stop after Phase 1 if the only goal is "one unit to deploy"; continue to Phase 4 only if the goal is one codebase with one source of truth and hot reload.

One important nuance: **merging by itself does not improve connection stability.** The stability properties live in Viberoxy's path/drain/canary code. A single process can actually *reduce* stability if a UI panic, a config reload, or a 650k-entry state load can take down the proxy. The design in `6 is what makes the merged service more stable than the current split.

---

## 2. What exists today

### 2.1 Inventory

| | Viberoxy | Viberayd | Viber Console |
|---|---|---|---|
| Module | `viberoxy` | `github.com/amirrezaalavi/Viberayd` | `github.com/yolka-wiz/viber-console` |
| Go directive | 1.21.0 | 1.21.0 | 1.26 |
| Production Go LOC | 11,087 | 7,064 | 2,100 |
| Test Go LOC | 19,179 | 7,826 | 1,603 |
| Role | Data plane: fetch → speed-test → WAN xray pool → CONNECT/SOCKS | Subscription aggregator: fetch → test → `working.txt`/`/sub` + API | Supervisor + control plane + SPA |
| Entry point | `main.go` (`package main`) | `cmd/viberayd/main.go` | `cmd/console/main.go` |
| License | GPL-3.0 | GPL-3.0 | GPL-3.0 |

### 2.2 Runtime topology today

```
clients ──CONNECT/SOCKS──> viberoxy :1080 / :0
                              │
                              ├─ WAN xray slots 127.0.0.1:10700..10703
                              ├─ speed-test xray 127.0.0.1:10800..10823
                              ├─ control API :1980
                              └─ metrics/health :9090 (optional)
                              ▲
                              │  GET http://127.0.0.1:8080/sub  (base64 of working.txt)
                              │
                         viberayd :8080 /sub, :8081 /api
                              ├─ test xray pool 127.0.0.1:10820..10919
                              └─ state.json + working.txt

viber-console :8090
   ├─ polls viberayd :8081, viberoxy :9090 and :1980
   ├─ edits /etc/viber/viberayd.env and viberoxy.env
   └─ supervises both binaries (SIGINT → wait → restart)
```

Three processes, three config surfaces, three HTTP surfaces, two xray port pools, two definitions of "working".

### 2.3 Baseline verification

All three projects build and their tests pass on this machine:

```
$ go build ./...          # all three: exit 0
$ go test ./... -count=1
viberoxy     ok  66.9s   (15 packages)
viberayd     ok  29.2s   (12 packages)
viber-console ok  8.1s   (5 packages)
```

No pre-existing test failure blocks the merge. `xray 26.3.27` is available at `/usr/local/bin/xray`, so real end-to-end tests are possible.

---

## 3. Why the split is error-prone (evidence)

### 3.1 The config schema is already out of sync

The console's `internal/config/schema.go` defines **38 fields**; the two daemons actually read **60 knobs**.

- **23 real daemon knobs are missing from the UI**, including:
  - Viberoxy: `ALLOW_DEGRADED_FALLBACK`, `ALLOW_HTTP_SUBSCRIPTION`, `ALLOW_PRIVATE_TARGETS`, `ALLOW_PUBLIC`, `API_PORT`, `API_TOKEN`, `DOWNLOAD_ENDPOINT`, `DOWNLOAD_FALLBACK`, `DRAIN_MAX`, `HEALTH_ENDPOINTS`, `LISTEN_ADDR`, `MAX_TTFB_MS`, `NO_AFFINITY_DOMAINS`, `PROMOTE_PASSES`, `PROXY_USERS`, `REJECT_DIRECT_EXIT`.
  - Viberayd: `DAEMON_DOMAINS`, `DAEMON_DOMAIN_TIMEOUT`, `DAEMON_DOMAIN_QUORUM`, `DAEMON_DOMAIN_MIN_BYTES`, `DAEMON_STABILITY_PROBES`, `DAEMON_MAX_XRAY_TESTS`, `DAEMON_PORT_BASE`.
- **One field is stale:** `DAEMON_TCP_PING` is in the UI schema but no daemon reads it (the TCP gate is unconditional).
- **Defaults disagree:**
  - `XRAY_MUX`: UI default `true`; Viberoxy default `false`.
  - `DOWNLOAD_SIZE`: UI default `10,000,000`; Viberoxy default `1,000,000`.
  - `MINIMUM_SPEED`: UI type is integer; Viberoxy accepts a float down to `0.1`.
- **`API_PORT` is read directly from the environment in `main.go`**, is not part of `proxycfg.Config`, and is not in the UI schema.
- The two daemons validate differently: Viberoxy hard-exits on a bad value; Viberayd silently keeps the default (`LoadConfigFromEnv` ignores `Atoi` errors).

**Consequence:** the UI can show values the daemon is not using, and "Apply + restart" can put a daemon into a configuration the other daemon/UI does not describe.

### 3.2 The self-subscription hop is fragile

Viberoxy consumes Viberayd via `SUBSCRIBER_URL=http://127.0.0.1:8080/sub`. That creates a circular runtime dependency:

- Viberoxy refuses plain `http://` by default; someone must set `ALLOW_HTTP_SUBSCRIPTION=true`, which the console does not expose.
- Viberoxy's startup blocks until Viberayd has produced a usable `working.txt`; if Viberayd is restarting, Viberoxy starts with no WANs.
- The handoff is a base64-encoded file read on every request (`handleSub`), then parsed again by Viberoxy.
- The historical bug where latency was appended inside the share-link URI (fixed in `Viberayd-fix`) is a good example of how a file/format boundary turns a metadata change into a broken data plane.

In a merged service this hop disappears: the catalog feeds the WAN manager directly.

### 3.3 Double work and divergent semantics

| Concern | Viberoxy | Viberayd | Risk |
|---|---|---|---|
| Parsing | `internal/proxycfg/parse.go` | `pkg/parser/*` | Two grammars, two rejection policies, two models (Viberoxy keeps only protocol/server/port/name/raw; Viberayd keeps a full tagged union) |
| Xray render | `internal/xraycfg/build.go` (parses `Raw` again) | `internal/tester/xray_config.go` (renders from model) | A config can pass one and fail the other; known gaps differ (grpc `serviceName`, xhttp settings, reality modelling) |
| Fetch | `internal/subs` (8 MiB cap, https-only, ETag, conditional GET) | `pkg/fetcher` (no cap, no scheme policy) | Viberayd is the weaker path and is the one that touches arbitrary subscription URLs |
| Testing | speed + TTFB + stability | TCP/TLS/protocol/domain/stability | Up to two test passes and two xray subprocesses per config |
| Protocol support | skips `tuic`, `hysteria2`, `wireguard` at promotion (leak guard) | tests them; `tuic`/`hysteria2` always fail because xray has no outbound | UI can report "working" configs that can never become a WAN |

This is the core of "prevent errors": today the two halves can disagree about whether a link is valid, and the only thing preventing a direct-egress leak is a manual `IsXraySupported` guard in one path.

### 3.4 The default port ranges collide

- Viberoxy test allocator: `TEST_BASE_PORT` 10800 for `MAX_TEST_PER_CYCLE (20) + WAN_COUNT (4)` = **10800–10823**.
- Viberayd test pool: `DAEMON_PORT_BASE` 10820 for `PortManager(base, 100)` = **10820–10919**.

Overlap: **10820–10823**. Both sides check availability before binding, but it is a TOCTOU check (`net.Listen` then close, then xray binds later). When both cycles test concurrently, one xray intermittently fails with `address already in use`, which is recorded as a *config failure*, not an infrastructure error. The operator sees flaky nodes that are actually a port collision.

There is no global validation that `WAN_BASE_PORT + WAN_COUNT`, spares, `TEST_BASE_PORT`, `DAEMON_PORT_BASE`, `PROXY_PORT`, `SOCKS_PORT`, `METRICS_PORT`, `API_PORT`, and the console port are disjoint. A single service should validate this as one map at startup and on reload.

### 3.5 Restart-based config changes drop live connections

`viber-console` applies config by writing `viberoxy.env`/`viberayd.env` and, when `restart` is requested, calling `Service.Restart`: SIGINT, wait up to 5 s, start again.

- Viberoxy's shutdown closes the proxy server and calls `pool.ShutdownAll()`, so **every CONNECT/SOCKS tunnel dies**.
- Changing even `ACCESS_LOG` or `MINIMUM_SPEED` through the UI therefore costs an outage.
- During the gap, the other daemon keeps running against a half-restarted stack.
- The two binaries can be upgraded independently and drift in API/format compatibility.

This is the main operational argument for hot reload: today "config change" and "connection reset" are the same action.

### 3.6 State and concurrency issues that a merge must not inherit

- Viberayd's `Daemon` has a `StateMu`, but the cycle mutates `State.Configs` (`mergeWithParsed`, `ApplyResults`, `writeOutputFile`) **without taking it**. HTTP handlers (`/api/configs`, `/metrics`) read the same map under `RLock`. That is a data race waiting for a concurrent scrape; `go test -race` does not catch it because the tests do not run a cycle and an HTTP scrape together.
- Viberoxy's `NewAPIHandler` captures drop/replace options (`TestTimeout`, `DownloadSize`, `XrayMux`, `MinimumSpeed`, `MaxTTFB`) **at construction**. Even if the process could see a new config, the manual drop path would keep using the old values.
- Viberoxy's `runLoop` captures `FetchInterval` and `DRAIN_MAX` once; `canaryLoop` captures `KeepaliveInterval` and `HEALTH_ENDPOINTS` once; `startServices` captures all listener ports and `APIToken`; `auth.UsersFromEnv()` and `subs.AllowHTTPFromEnv()` re-read the process environment at request time. Hot reload requires replacing this mix with one provider.
- Viber Console's `Store.poll` holds its write mutex across two network calls (up to 5 s each), so UI reads can block behind a slow daemon.

### 3.7 Security posture is inconsistent

- **Console:** `CONSOLE_LISTEN` defaults to `:8090` (all interfaces); `CONSOLE_TOKEN` empty means **no auth check at all** (the comment says "localhost-only", but there is no host check). `/api/control/restart` and `/api/config/values` are reachable without credentials if the port is reachable.
- **Viberayd:** binds `:8080` and `:8081` on all interfaces, no auth; `/api/urls` can add/replace subscription URLs, `/api/cycle/trigger` can start work.
- **Viberoxy:** the only hardened one: loopback default, `PROXY_USERS`, `API_TOKEN`, bind gate, SSRF guard, https-only subscriptions.

A merged service must adopt the Viberoxy posture for every listener, not the weakest one.

---

## 4. Target architecture

### 4.1 Principles

1. **One schema, one config, one state directory, one control plane.**
2. **One parser, one model, one xray renderer, one test pipeline, one port allocator.**
3. **The data plane never waits on the control plane.** UI/API/cycle failures must not stop proxying.
4. **Config changes are transactional and never interrupt in-flight work.**
5. **xray stays a supervised child process**; Go code stays in one process.
6. **Keep the zero-dependency property**; implement file watching with a 1 s stat/hash poll rather than adding `fsnotify`.

### 4.2 Proposed layout

```
viber/
  go.mod                          # module github.com/amirrezaalavi/viber (or your org)
  cmd/viber/main.go               # single entry point: parse flags, load config, run
  internal/settings/              # schema, parse, validate, atomic snapshot, apply engine
  internal/lifecycle/             # component supervisor, signal handling, ordered shutdown
  internal/contract/              # shared domain: ProxyConfig, Entry, Snapshot, TestResult
  internal/catalog/               # subscription URLs, raw configs, state, dedupe, persistence
  internal/subscription/          # fetcher + scheduler + test pipeline (from Viberayd)
  internal/proxy/                 # WAN pool, CONNECT/SOCKS, health, selection, metrics (from Viberoxy)
  internal/console/               # dashboard API, control API, embedded SPA (from Viber Console)
  internal/platform/              # atomicfile, logging, httputil, netutil, portmap
  pkg/parser/                     # the one share-link parser
  pkg/xraycfg/                    # the one xray JSON renderer
  pkg/xrayproc/                   # xray process lifecycle (already exists)
```

Migration mechanics:

- Move `Viberoxy-fix/*.go` (package `main`) into `internal/proxy`, rename the package, export `Run(ctx, deps)`, and replace the globals with fields on a `Service` struct.
- Move `Viberoxy-fix/internal/*` to `internal/proxy/*` (or `pkg/*` for the reusable ones).
- Move `Viberayd-fix/internal/{daemon,tester,concurrency,cache,models,...}` and `pkg/{parser,fetcher}` into `internal/subscription` and `pkg/{parser,fetcher}`.
- Move `viber-console/internal/{dashboard,collector,config,supervisor}` into `internal/console`; delete `supervisor` after Phase 1.
- Rewrite the three module import paths to the new module; this is mechanical but touches every file.
- Target Go 1.22–1.24 unless a 1.26 feature is actually needed (none of the three requires 1.26; the console's `go 1.26` directive is stricter than its code).

### 4.3 Components

```
┌──────────────────────────────────────────────────────────────────┐
│ viber (one process)                                              │
│                                                                  │
│  settings.Manager ──snapshot──> every component                  │
│       ▲                                                          │
│       │ file poll / SIGHUP / PUT /api/config                     │
│                                                                  │
│  console (HTTP :8090) ── UI, /api/*, /metrics, /healthz, /readyz │
│       │                         /sub                             │
│       │ in-process snapshots                                     │
│       ▼                                                          │
│  catalog <──── subscription loop (fetch + test + state)          │
│       │                                                          │
│       │ candidates                                               │
│       ▼                                                          │
│  proxy.Service ── WANPool ── xray child per slot                 │
│       │              │                                           │
│       └── CONNECT :1080 / SOCKS :1080+1 (optional)               │
│                                                                  │
│  lifecycle: supervise components, ordered shutdown, panic guard  │
└──────────────────────────────────────────────────────────────────┘
```

### 4.4 Data flow after the merge

```
subscription URLs ──fetch (hardened)──> catalog (raw + metadata, deduped by sha256)
      │
      ├─ TCP prefilter (bounded)
      ├─ qualify once: TCP/TLS/protocol + one real proxied request + speed/TTFB
      │     └─ stability probes (N) only for passers
      ▼
catalog state (unknown/working/failed/unreachable) + candidate pool
      │
      ├─ WAN promotion: fill empty slots / rotate with hysteresis
      │     └─ xray child starts on a spare port; make-before-break Swap
      ▼
WANPool (active/draining, per-path inflight/health/EWMA)
      │
      └─ CONNECT/SOCKS selection (affinity + P2C + retry) ── client
```

The double test disappears: one qualification pass produces both "is this config good?" and "how fast is it / what is its TTFB?", and the WAN manager consumes that result directly. `working.txt` becomes an **export** for external clients, not the internal transport. `/sub` is served from the catalog in memory (optionally still written to disk for compatibility).

### 4.5 Process and listener model

- One process, one PID, one log stream with `component=proxy|catalog|console|xray` fields.
- xray children: one per active WAN + bounded test workers. Use `Setpgid` + `Pdeathsig` on Linux so a parent death cannot leave orphan xray processes.
- Control-plane listener: one `http.Server` on `127.0.0.1:8090` for UI, `/api/*`, `/metrics`, `/healthz`, `/readyz`, `/sub`.
- Data-plane listeners: CONNECT proxy `127.0.0.1:1080`, optional SOCKS5. These must remain separate from the UI/control server (CONNECT semantics conflict with normal HTTP routes).
- Legacy compatibility listeners (8080 `/sub`, 8081 API, 9090 metrics, 1980 API) can be enabled by a `compat` flag during migration, then removed.
- All listeners bind loopback by default. A non-loopback bind requires `PROXY_USERS`/`API_TOKEN` or an explicit `ALLOW_PUBLIC=true`, exactly like Viberoxy today.
- One `portmap` validation at startup and on config apply: proxy, socks, control, metrics, WAN range, spare range, test range, viberayd-compatible range (if kept) must be pairwise disjoint and inside 1..65535.

### 4.6 HTTP/API compatibility

Keep the existing paths as aliases so the current UI and scripts keep working:

| Path | New owner | Behaviour |
|---|---|---|
| `GET /` + `/static/*` | console | SPA (unchanged) |
| `GET /api/health` | console | liveness |
| `GET /api/overview` | console | in-process snapshot (same JSON) |
| `GET /api/viberayd/*` | catalog adapter | same shapes, no HTTP hop |
| `GET /api/viberoxy/*` | proxy adapter | same shapes, no HTTP hop |
| `GET /sub` | catalog | base64 of working raw links (compat) |
| `GET /metrics` | one registry | `viber_*` + legacy `viberoxy_*`/`viberayd_*` aliases |
| `GET /healthz`, `/readyz` | lifecycle | process / at-least-one-routable-WAN |
| `GET/PUT /api/config`, `/api/config/schema`, `/api/config/apply` | settings | new, transactional |

---

## 5. Hot reload design

### 5.1 Semantics (what "hot" means)

Define four apply modes and show them in the UI:

| Mode | Meaning | In-flight impact |
|---|---|---|
| **live** | Swap an immutable snapshot; next read uses it | None |
| **next-cycle / next-test** | New value is used when the next cycle/test starts; the current one finishes with the old value | None |
| **rolling** | Reconcile a component one unit at a time (e.g. WANs) with make-before-break | None; brief extra capacity/process |
| **restart-required** | Rejected for live apply; needs a process restart | Full outage, explicit |

Principles:

- **No partial apply.** Parse and validate the whole file; if any field is invalid, reject the whole update and keep the old snapshot. Return per-field errors.
- **No in-flight interruption.** A connection snapshots the config it needs at accept time; a cycle snapshots at cycle start. A reload never calls `ShutdownAll`, closes a listener with active connections, or stops a WAN carrying traffic.
- **Monotonic generation.** Each accepted update gets a generation number. Components log `config_generation=N`; metrics expose it.
- **Idempotent.** Re-applying the same file is a no-op.
- **Auditable.** Record who/when/what changed (file, SIGHUP, API), old/new values with secrets redacted, and the apply result.

### 5.2 One schema to rule all three

Replace `console/internal/config/schema.go` + `proxycfg.ParseConfig` + `daemon.LoadConfigFromEnv` with one declarative schema:

```go
type ApplyMode int
const (
    Live ApplyMode = iota
    NextCycle
    Rolling
    RestartRequired
)

type Field struct {
    Key      string   // canonical, e.g. "proxy.minimum_speed_mbps"
    Aliases  []string // legacy env names, e.g. "MINIMUM_SPEED"
    Section  string
    Type     Type     // string/int/float/bool/duration/enum/list/secret
    Default  string
    Min, Max *float64
    Enum     []string
    Secret   bool
    Apply    ApplyMode
    Help     string
    Validate func(string) error
}
```

The same slice drives:

- config-file parsing and alias migration,
- startup validation,
- the `/api/config/schema` JSON consumed by the SPA,
- generated documentation,
- a test that **every** `Config` field has exactly one schema entry and every schema key is read somewhere (this is the guard against the current 23-key drift).

Config file format recommendation: a single `viber.conf` with dotted keys, comments, and one `KEY=VALUE` per line (stdlib-only, familiar to the current env files):

```ini
# /etc/viber/viber.conf
server.listen           = 127.0.0.1:8090
proxy.proxy_port        = 1080
proxy.wan_count         = 4
proxy.test_base_port    = 10800
subscription.urls_file  = /etc/viber/urls.txt
subscription.interval   = 300s
subscription.depth      = standard
log.level               = info
```

Environment variables override the file for container/secret use (`VIBER_PROXY_API_TOKEN`, etc.). Legacy names (`SUBSCRIBER_URL`, `WAN_COUNT`, ...) are accepted as aliases for one release, with a deprecation warning.

### 5.3 Reload engine

```
watch (1s stat+hash) / SIGHUP / PUT /api/config
        │
        ▼
parse + alias-migrate + validate (cross-field, ports, files, auth)
        │  invalid → 400 + per-field errors, old snapshot untouched
        ▼
plan (diff old vs new → ordered reconcile steps)
        │
        ▼
apply with rollback:
  1. settings snapshot swap (live fields)
  2. auth/router/log-level swaps (atomic pointers)
  3. timers (fetch/canary) reset
  4. listener reconcile (start new before stopping old)
  5. WAN reconcile (grow/shrink/rolling recycle)
  6. persist new config + generation
        │  any step fails → undo started listeners/processes, restore old snapshot
        ▼
report per field: applied | pending next cycle | rolling | restart required | rejected
```

Implementation sketch:

```go
type Manager struct {
    cur  atomic.Pointer[Snapshot]
    ch   chan struct{} // closed/replaced on change, wakes loops
    mu   sync.Mutex   // serialises applies
}

func (m *Manager) Current() *Snapshot { return m.cur.Load() }

func (m *Manager) Apply(next *Snapshot) (Report, error) {
    // validate/plan before touching the running service
    // swap, then run reconcilers in order; rollback on error
}
```

- `Snapshot` is deeply immutable (slices copied, no pointers to mutable structs).
- Per-connection code calls `settings.Current()` once and uses that value for the lifetime of the connection.
- Long loops use a resettable timer plus a `changed` channel, not a captured `time.Duration`:

```go
for {
    cfg := settings.Current()
    timer := time.NewTimer(cfg.FetchInterval)
    select {
    case <-ctx.Done(): return
    case <-timer.C: runCycle(cfg)
    case <-settings.Changed(): timer.Stop()
    }
}
```

- Per-cycle code takes one snapshot at cycle start; a reload during the cycle cannot change the semantics halfway through.

### 5.4 Listener reconcile

For a port change (`PROXY_PORT`, `SOCKS_PORT`, `METRICS_PORT`, `API_PORT`, `CONSOLE_LISTEN`):

1. Bind the new listener first. If it fails, abort the update and keep the old config (no downtime).
2. Publish the new handler/listener.
3. Close the old **listener** (stop accepting), but do not call `http.Server.Shutdown` on it; accepted CONNECT connections continue.
4. Track hijacked connections explicitly (`connTracker` increment/decrement), and close the old server only after `inflight == 0` or `DRAIN_MAX`.

For `LISTEN_ADDR` (same port, new address), the old socket must be released before the new bind. Mark this **restart-required** initially; if zero downtime is mandatory, implement a two-phase `SO_REUSEPORT` bind on Linux (with a short overlap), but that is extra risk for a rare change.

### 5.5 WAN reconcile

- **Grow** (`WAN_COUNT` up): append slots, test candidates, promote with the existing make-before-break path. Capacity increases; no existing WAN is touched.
- **Shrink** (`WAN_COUNT` down): drain the highest-index slots; in-flight connections finish; no new connections are routed there. Never shrink below 1 without an explicit "drain all" action.
- **XRAY_MUX change:** existing WANs keep their process; new/replacement WANs use the new value. For an immediate consistent state, recycle one WAN at a time (start replacement on a spare port, verify, swap, drain old). This is a rolling apply.
- **WAN_BASE_PORT change:** apply to the next replacement (rolling) rather than restarting every WAN at once. The control API must not assume base+index for existing slots; use each slot's `ServicePort`.
- **TEST_BASE_PORT change:** build a new allocator at the next cycle boundary; in-flight tests finish on the old allocator. Validate the new range against WAN/spare/listener ports before accepting.

This is where Viberoxy's existing `path.Path` generation identity pays off: shrinking/replacing a slot never invalidates a connection that already reserved the old path.

### 5.6 Hot-reload matrix

| Area | Keys | Mode | Notes |
|---|---|---|---|
| Logging | `LOG_LEVEL`, `LOG_FORMAT` | live | `slog.LevelVar`; format change may need restart |
| UI/auth | `CONSOLE_TOKEN`, `API_TOKEN`, `PROXY_USERS` | live | atomic verifier; new connections use new creds |
| SSRF | `ALLOW_PRIVATE_TARGETS` | live | read per connection |
| Routing | `ROUTE_MODE`, `DIRECT_DOMAINS`, `PROXY_DOMAINS`, `*_LIST_FILE` | live | build new `Router`, atomic swap; existing connections keep route |
| Access log | `ACCESS_LOG` | live | atomic bool |
| Selection | `WAN_FAIL_THRESHOLD`, `ALLOW_DEGRADED_FALLBACK` | live | read per decision |
| Health | `KEEPALIVE_INTERVAL`, `HEALTH_ENDPOINTS`, `DRAIN_MAX` | live | reset timer; future probes/drains |
| Fetch | `SUBSCRIBER_URL`/URL list, `ALLOW_HTTP_SUBSCRIPTION`, `FETCH_INTERVAL` | live / next cycle | timer reset; next fetch |
| Speed test | `TEST_TIMEOUT`, `DOWNLOAD_SIZE`, `DOWNLOAD_ENDPOINT`, `DOWNLOAD_FALLBACK`, `MINIMUM_SPEED`, `MAX_TTFB_MS`, `PROMOTE_PASSES`, `STABILITY_PROBES`, `MAX_TEST_PER_CYCLE` | next test/cycle | snapshot at cycle start; active WANs unaffected |
| Catalog test | `DEPTH`, `PARALLEL`, `TIMEOUT`, `DOMAINS`, `DOMAIN_*`, `MAX_XRAY_TESTS`, `RETEST_INTERVAL`, `KEEP_SUCCESSFUL`, `MAX_LATENCY_MS` | next cycle | snapshot at cycle start |
| WAN count | `WAN_COUNT` | rolling | grow/shrink with drain; never below 1 |
| Xray mux | `XRAY_MUX` | rolling | recycle one WAN at a time, or apply to new WANs |
| Test ports | `TEST_BASE_PORT`, `DAEMON_PORT_BASE` | next cycle | new allocator at cycle boundary; validate disjoint |
| WAN base port | `WAN_BASE_PORT` | rolling / next replacement | existing WANs keep their port |
| Listener ports | `PROXY_PORT`, `SOCKS_PORT`, `METRICS_PORT`, `API_PORT`, `CONSOLE_LISTEN` | listener swap | start new, stop accepting old, drain |
| Bind address | `LISTEN_ADDR`, `ALLOW_PUBLIC` | restart-required | must re-bind every listener; validate auth |
| Paths | `URLS_FILE`, `OUTPUT_FILE` | next cycle | `STATE_FILE`/`CONFIG_PATH` restart-required |
| Xray binary | `XRAY_BIN` | restart-required (or rolling only if version-compatible) | old WANs would otherwise run a different binary |
| Boot only | `ALLOW_DEGRADED_BOOT` | boot-only | stored for next boot; no runtime effect |

Result: roughly **85–90% of knobs apply without a process restart**; the rest are rare, structural, and can be explicitly labeled in the UI.

### 5.7 Validation that must run before any apply

- All listener ports are valid, unique, and disjoint from WAN/spare/test ranges.
- `TEST_BASE_PORT + maxTests` and `WAN_BASE_PORT + WAN_COUNT + spares` stay within 1..65535.
- `WAN_COUNT` is within the supported 1..5 (or raise the limit deliberately).
- Non-loopback `LISTEN_ADDR` has `PROXY_USERS`/`API_TOKEN` or `ALLOW_PUBLIC=true`.
- Subscription URLs are valid; plain `http://` is only allowed with the explicit opt-in; no URL points back at this service (`/sub` self-loop guard).
- Files/directories are writable; secrets are not logged.
- Test/domain URLs are valid and use permitted schemes.
- `DRAIN_MAX`, timeouts, intervals are positive and within bounds.
- Changing a value cannot lock the operator out (e.g. turning off `ALLOW_PUBLIC` while bound publicly must be rejected or force a rebind).

---

## 6. Connection stability

### 6.1 Preserve what already works

Do not rewrite these; move them and keep their tests:

- `internal/path`: one generation per xray lifetime; inflight reservations survive replacement.
- `WANPool.Swap` / `DropAndReplace`: make-before-break; old process keeps serving until the new path is active.
- Draining: `inflight == 0` or `DRAIN_MAX`, health drains reversible, replacement drains not.
- Canary loop: per-path probes, environmental breaker (≥75% failing), dead-tunnel detection, refill trigger.
- Passive health + sched/affinity/retry: selection quality and failover.
- `xrayproc`: Wait-based liveness and zombie reaping.

### 6.2 New risks from one process, and mitigations

| Risk | Why it is new | Mitigation |
|---|---|---|
| Blast radius | A panic in UI/catalog kills the proxy | `recover` at every goroutine boundary (connection handlers, cycle workers, HTTP handlers, reconcilers); component supervisor restarts a failed component; no `os.Exit`/`slog.Fatal` in libraries |
| GC/memory | 647k catalog entries in one heap can cause latency spikes | Store compact entries (hash + raw + metadata); parse on demand for candidates; cap catalog; do not keep two parsed maps; benchmark 650k; tune `GOGC`/`GOMEMLIMIT` |
| CPU contention | Tester and proxy share one process | Bounded test workers (`MAX_XRAY_TESTS`, `PARALLEL`), separate semaphores, optional `nice`/cgroup; never test unbounded |
| FD limits | Proxy conns + control + xray pipes in one PID | Raise `LimitNOFILE`, close on all paths, expose fd gauge; test under load |
| Shared locks | A global config/store mutex across I/O stalls the data plane | Immutable snapshots; never hold a lock across network/disk; per-slot locks only |
| Config reload | A bad apply can break the running service | Transactional apply + rollback + last-known-good; reject invalid whole-file |
| Control plane | UI/API load can starve proxy | Separate server, timeouts, pagination, no giant JSON dumps under lock |
| Upgrade | One binary restart still drops tunnels | Graceful drain window; config reload avoids restarts; systemd `KillMode=mixed`, `TimeoutStopSec` |

### 6.3 Startup and shutdown

**Startup (fast, degraded-first):**

1. Parse config + validate port map; acquire a single-instance lock; open state.
2. Start control server and `/healthz` (ready=false).
3. Load last-known-good WAN set from state and start those xray processes immediately; verify with a quick canary; serve as soon as one is routable (`ALLOW_DEGRADED_BOOT` semantics).
4. Start the catalog/subscription loop in the background.
5. Mark ready when at least one WAN is routable; expose per-component status in the UI.

This is a real availability win over today, where Viberoxy blocks startup until it has fetched and tested enough configs.

**Shutdown (ordered, bounded):**

1. `ready=false`; stop accepting new connections (close listeners).
2. Cancel catalog cycle and test workers; wait with a deadline.
3. Drain front-end connections up to `DRAIN_MAX` (configurable, default 30–60 s for shutdown; 600 s for replacement).
4. Drain WANs, stop xray children (SIGTERM → SIGKILL after grace), remove temp configs.
5. Save state/config atomically; stop control server; flush logs.

A second SIGINT/SIGTERM should skip the drain and exit immediately (operator escape hatch).

### 6.4 Failure matrix

| Failure | Expected behaviour |
|---|---|
| One xray WAN dies | Canary detects dead tunnel, fails/drains slot, triggers refill; other WANs keep serving |
| All WANs dead | `readyz` 503; refill cycles with backoff; if `ALLOW_DEGRADED_FALLBACK=false`, fail fast rather than leak direct |
| Subscription fetch fails/timeouts/oversize | Keep last good catalog/WANs; backoff/jitter; never clear the pool |
| Subscription returns garbage | Reject the fetch; keep previous configs; log reason |
| Config file invalid | Reject apply; keep running config; report field errors |
| Config file deleted | Keep last-known-good; warn; optionally restore from `.bak` |
| State file corrupt | Start empty + rebuild from subscription; keep a `.bak`; never crash-loop |
| Disk full | State save fails → keep serving from memory; warn; do not corrupt old file (tmp+rename) |
| Control plane panic | Recover per request; process stays up; proxy unaffected |
| Catalog panic | Component supervisor restarts it with backoff; proxy keeps serving |
| Port already in use at startup | Fail with a clear error naming the port and the conflicting range |
| Port taken on reload | Abort apply; keep old listener/config |
| OOM | `GOMEMLIMIT` + catalog cap; reduce test concurrency; alert |
| Parent killed | `Pdeathsig`/process group ensures xray children do not orphan |

### 6.5 Test plan (must be part of the work, not an afterthought)

- `go test -race ./...` across the merged module; add a test that runs a catalog cycle concurrently with `/api/configs` and `/metrics` (this would have caught `3.6).
- Reload-under-load: a long-lived CONNECT download (minutes, multi-GB) while applying every **live** field one by one; assert zero bytes lost and the connection stays open.
- Rolling changes: shrink/grow `WAN_COUNT` under load; recycle `XRAY_MUX`; change `WAN_BASE_PORT`; assert no drop and no slot below the desired count for more than the swap window.
- Listener swap: change `PROXY_PORT`/`API_PORT` under load; assert old connections finish, new ones use the new port, and a failed bind leaves the old listener intact.
- Chaos: kill xray processes, block health endpoints, 50% packet loss, subscription 500/timeout/oversized, invalid config, disk full.
- Soak: 72 h with periodic reloads; track RSS, goroutines, fds, xray child count, connection success rate, p50/p95 TTFB.
- Memory/perf: import a 650k-entry state; measure load time/RSS; compare proxy latency against the current 3-process baseline.
- Real xray: golden `xray -test` for every supported protocol/transport combination from both old parsers.

Suggested acceptance SLOs:

- Live/rolling apply: **zero** dropped in-flight connections.
- `readyz` never reports ready with zero routable WANs.
- Recovery from a killed WAN: < 30 s with a cached candidate (or one cycle otherwise).
- Config apply for live fields: < 1 s, no proxy latency regression.
- 72 h soak: RSS stable within ±10%, no goroutine/fd growth, no orphan xray.

---

## 7. Options

| Option | What it is | Effort | Meets "one service" | Meets hot reload | Stability |
|---|---|---|---|---|---|
| **A. Monorepo only** | One repo/CI, still 3 binaries/processes | 1–3 days | No | No | Unchanged |
| **B. One binary, multi-process** | `viber` supervises itself as `viber-viberoxy`/`viber-viberayd`; one unit/container | 2–4 weeks | Operationally yes | Partial (per-daemon APIs) | Good (process isolation) |
| **C. True single process** | One Go process, shared config/catalog, in-process data path | 6–10 weeks MVP, 12–20 full | Yes | Yes, for ~90% of knobs | Good if hardened; higher blast radius |

Option B still requires moving Viberoxy out of `package main` to call it, so it is not a shortcut around the first refactor. But it is a useful **stopping point**: one artifact and one unit, with fault isolation preserved, before sharing state.

**Recommendation:** Option A immediately (cheap), then Option C via the phased plan, using Option B as the Phase 1 checkpoint. If the team's priority is "one deployable with minimal risk" and hot reload can wait, stop at B.

---

## 8. Phased plan

| Phase | Duration | Deliverables | Exit criteria |
|---|---|---|---|
| **0. Baseline** | 2–3 days | New `viber` repo; import all three at baseline commits; one module; CI builds/tests/race; architecture + config docs; upstream remotes preserved | All existing tests pass from one checkout; binary builds |
| **1. One binary, same behaviour** | 2–4 weeks | `cmd/viber`; move Viberoxy root into `internal/proxy` and export `Run`; move Viberayd into `internal/subscription`; console in `internal/console`; in-process lifecycle replaces `supervisor`; one signal handler/ordered shutdown; one log stream | One PID serves proxy, `/sub`, UI; SIGTERM clean; smoke tests pass; no port conflicts; behaviour parity |
| **2. Unified config + live fields** | 3–4 weeks | One schema/file/loader; legacy env alias migration; atomic `settings.Manager`; live apply for auth/router/log/thresholds/intervals/test knobs; generated UI schema; `/api/config/*`; file watch + SIGHUP; audit/rollback | Change every **live** field without restart; long-lived CONNECT survives; invalid config rejected atomically; no config drift test failures |
| **3. In-process data path + catalog** | 3–4 weeks | Catalog owns URLs/state/candidates; proxy consumes it directly; `/sub` from memory; remove self-subscription and `working.txt` as transport; console reads snapshots in-process; prewarm last-known-good WANs | Core flow has no loopback HTTP/file dependency; restart preserves WAN set; 650k-entry benchmark; `/sub`/API compatibility |
| **4. One parser/renderer/pipeline** | 4–6 weeks | Canonical `ProxyConfig`; one parser with corpus from both; one `xraycfg` with options; one qualification pipeline (TCP/TLS/protocol/speed/TTFB/stability); one port allocator; protocol support matrix | Same link → same outbound; `xray -test` golden suite; no false "working"; no direct-egress path; double testing removed |
| **5. Rolling reload + hardening** | 3–4 weeks | WAN grow/shrink/mux recycle; listener swap/drain; panic isolation; watchdogs; resource limits; fd/memory metrics; 72 h soak; chaos suite | Zero-drop rolling/live apply; no leaks; recovery SLOs; `readyz` accurate |
| **6. Migration + packaging** | 2–3 weeks | `viber migrate` from old env files; systemd unit/Dockerfile/compose; legacy port/path aliases; rollback guide; runbooks | One-command migration; old `/sub` URL works; rollback tested; docs complete |

**Total:** ~12–19 engineer-weeks for one engineer; ~7–11 calendar weeks with two engineers working in parallel (parser/renderer vs lifecycle/config), with Phase 4 as the integration bottleneck.

You can stop after Phase 1 (single unit, unchanged behaviour) or Phase 2 (hot reload without full data-path unification) if priorities change. Phases 3–4 are what remove the duplicate work and the divergent semantics.

---

## 9. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Parser/renderer regressions | High | High | Golden corpus from both projects; differential tests; `xray -test`; staged rollout; keep old binaries for rollback |
| Hidden behaviour differences (grpc/xhttp/reality) | High | High | Protocol matrix; end-to-end tests per transport; compare old vs new qualification on the same subscription |
| Memory blow-up at 650k configs | Medium | High | Compact catalog; parse on demand; cap; benchmark; `GOMEMLIMIT`; avoid duplicate maps |
| One-process blast radius | Medium | High | Panic recovery, component supervisor, no fatal calls in libraries, chaos tests |
| Listener zero-downtime complexity | Medium | Medium | Start-before-stop; conn tracker; mark `LISTEN_ADDR`/same-port changes restart-required |
| Scope creep | High | Medium | Phase gates; stop after Phase 1/2 if goals met; explicit non-goals |
| Upstream divergence | Medium | Medium | Decide fork vs monorepo sync early; keep upstream remotes; document divergence |
| Config migration mistakes | Medium | Medium | Dry-run migrate; diff; `.bak`; reject-on-conflict; rollback |
| Test suite runtime/flakiness | Medium | Low | Keep build tags; parallelize; quarantine network tests; CI budget |
| Security regression | Low–Medium | High | One bind/auth policy; loopback default; token required for non-loopback; audit endpoints |

---

## 10. Migration and compatibility

- **Config:** ship a `viber migrate` command that reads `viberayd.env` + `viberoxy.env` + console env, maps legacy names via schema aliases, detects conflicting values (e.g. two `FETCH_INTERVAL`s), writes `viber.conf` + a backup, and prints a diff. Refuse to overwrite without `--force`.
- **Ports:** keep `PROXY_PORT` (1080), `/sub` (8080), console (8090), metrics (9090), API (1980) as compatibility aliases for one release. The new default can consolidate `/sub` onto the control port, but only after clients migrate.
- **State:** import existing `state.json` into the catalog (versioned schema). Keep the old file untouched; write the new store under a new name until cutover.
- **APIs:** keep `/api/viberayd/*` and `/api/viberoxy/*` paths and JSON shapes as adapters. Additive fields only; no breaking renames.
- **Rollback:** old binaries and env files remain installed; new service uses a separate state directory during canary; a `viber rollback` or systemd unit switch restores the old stack.
- **Packaging:** one unit/container; `LimitNOFILE=65535`; `KillMode=mixed`; `TimeoutStopSec=60`; healthcheck `/readyz`; non-root user; config 0640, state 0750, secrets 0600.

---

## 11. Open decisions

1. **Process model:** is "single service" satisfied by one artifact + one systemd unit with child processes (Option B), or must it be one OS process (Option C)?
2. **Refactor appetite:** Option C requires moving Viberoxy out of `package main` and touching its 11k production LOC + 19k test LOC. Is that acceptable, or is a wrapper/minimal-risk path preferred?
3. **Protocol scope:** keep `tuic`/`hysteria2` as "known unsupported" (current Viberoxy behaviour), or add a sing-box sidecar to actually test them? The latter adds another process/binary and is a separate project.
4. **Zero dependencies:** keep it (recommended) and implement file watching with polling? Or allow `fsnotify`/TOML for convenience?
5. **Compatibility window:** how long must old env files, ports 8080/8081/9090/1980, and the console's current API survive?
6. **Memory budget:** what is acceptable RSS for the live catalog (~650k configs)? This drives catalog design (full parse vs on-demand).
7. **Upstream strategy:** fork into a new `viber` repo (likely, because the merge changes Viberoxy structurally), or keep a monorepo that periodically syncs upstream?
8. **Restart-required set:** confirm that `LISTEN_ADDR`, `STATE_FILE`, `CONFIG_PATH`, `XRAY_BIN` are allowed to require a restart; everything else can be live/rolling.

---

## Appendix A — Evidence collected

```text
Go 1.26.6 linux/amd64
xray 26.3.27 (Xray-core)
ulimit -n: 1048576; nproc: 10

go build ./...                 # Viberoxy-fix, Viberayd-fix, viber-console: exit 0
go test ./... -count=1         # all green
  Viberoxy-fix      ok 66.864s
  Viberayd-fix      ok 29.154s (internal/tester)
  viber-console     ok  8.130s (internal/supervisor)

Config surface:
  console schema fields: 38 (15 viberayd, 23 viberoxy)
  actual daemon knobs:   60 (39 viberoxy, 21 viberayd)
  missing from schema:   23
  stale in schema:        1 (DAEMON_TCP_PING)
  default drift:          XRAY_MUX (true vs false), DOWNLOAD_SIZE (10MB vs 1MB),
                          MINIMUM_SPEED (int vs float 0.1+)

Port defaults:
  viberoxy test range: 10800..10823  (MAX_TEST_PER_CYCLE 20 + WAN_COUNT 4)
  viberayd test range: 10820..10919  (PortManager(10820, 100))
  overlap:             10820..10823
```

Key files inspected:

- `Viberoxy-fix/main.go`, `internal/proxycfg/config.go`, `internal/proxycfg/parse.go`, `internal/xraycfg/build.go`, `wan.go`, `proxy.go`, `socks.go`, `health.go`, `api.go`
- `Viberayd-fix/internal/daemon/{config,loop,http,state,tester}.go`, `internal/tester/{pipeline,xray_config}.go`, `pkg/{parser,fetcher}`
- `viber-console/internal/config/{schema,store}.go`, `internal/supervisor/supervisor.go`, `internal/dashboard/{store,control}.go`, `internal/collector/*`, `cmd/console/main.go`

## Appendix B — Proposed package mapping

| Current | New |
|---|---|
| `Viberoxy-fix/*.go` (main) | `internal/proxy/*.go` (package proxy) |
| `Viberoxy-fix/internal/{path,health,sched,affinity,retry,cands,auth,relayio,subs,ports,xrayproc}` | `internal/proxy/...` or `pkg/...` for reusable |
| `Viberoxy-fix/internal/{proxycfg,xraycfg}` | `pkg/parser`, `pkg/xraycfg` (unified) |
| `Viberayd-fix/internal/daemon` | `internal/subscription` |
| `Viberayd-fix/internal/{tester,concurrency,cache,models,errors,orchestrator}` | `internal/subscription/...` / `internal/contract` |
| `Viberayd-fix/pkg/{parser,fetcher}` | `pkg/parser`, `internal/subscription/fetch` (hardened) |
| `viber-console/internal/dashboard` | `internal/console/dashboard` |
| `viber-console/internal/collector` | `internal/console/adapters` (in-process snapshots) |
| `viber-console/internal/config` | `internal/settings` (schema/store) |
| `viber-console/internal/supervisor` | deleted after Phase 1 (replaced by `internal/lifecycle`) |
| `cmd/console`, `cmd/viberayd` | `cmd/viber` only |

---

## Bottom line

The merge is **possible and sensible**, but the valuable part is not moving files: it is collapsing three config systems, two parsers, two xray renderers, two test pipelines and two port allocators into one, then making config changes transactional and non-disruptive. That is a multi-week engineering project, not a weekend refactor.

If the goal is **one deployable with minimal risk**, do Phase 0 + Phase 1 (2–4 weeks) and stop.
If the goal is **one codebase, one source of truth, and no-restart config changes**, do Phases 0–5 (roughly 12–19 engineer-weeks) and keep Viberoxy's data-plane machinery intact while replacing everything around it.
