# Single service, modular and hot-reloadable — design

**Goal:** one binary, one process, one config, one state directory. Viberoxy's
data plane, Viberayd's catalog/qualification, and the console's UI run as
**in-process modules** that can be started, stopped, and reconfigured
independently.

## Reality check: what "hot reload" can mean in Go

Go cannot swap compiled code inside a running process. So "hot reload" is
defined precisely as:

| Kind | Supported? | Mechanism |
|---|---|---|
| **Config hot reload** | yes | immutable snapshot + atomic pointer swap; modules reconcile |
| **Module restart** | yes | supervisor stops/starts one module without touching the others |
| **Rolling replacement** | yes | WAN slots / test workers replaced one at a time (make-before-break) |
| **Binary upgrade** | graceful re-exec | listen sockets handed off, old process drains, new process takes over |
| **Code hot swap** | no | requires `plugin` (fragile, Linux-only) — not recommended |

The first three remove the operational pain (config change = outage). The
fourth is a later, optional upgrade path.

## Target layout

```
viber/
  go.mod                         # one module, zero external deps
  cmd/viber/main.go              # the only main()
  internal/
    contract/                    # shared domain types, no logic
      proxy.go                   #   ProxyConfig, share-link model
      entry.go                   #   catalog entry, state, latency
      snapshot.go                #   immutable views handed between modules
      result.go                  #   TestResult, outcome
    settings/                    # one schema, one file, one snapshot
      schema.go                  #   declarative fields + aliases + apply mode
      load.go                    #   file + env + legacy alias migration
      validate.go                #   cross-field, port map, auth, paths
      manager.go                 #   atomic.Pointer[Snapshot], generation, Apply
    lifecycle/                   # module supervisor
      module.go                  #   Module interface
      supervisor.go              #   start order, panic recovery, restart, stop
      shutdown.go                #   ordered drain
    catalog/                     # subscription → catalog → candidates
      fetch.go                   #   hardened fetch (from Viberoxy internal/subs)
      parse.go                   #   one parser (from pkg/parser + proxycfg)
      state.go                   #   state.json, working set, dedupe
      sub.go                     #   /sub export from memory
    qualify/                     # one qualification pipeline
      pipeline.go                #   TCP → TLS → protocol → xray → speed/TTFB → stability
      worker.go                  #   bounded workers, one port allocator
    proxy/                       # data plane (from Viberoxy package main)
      service.go                 #   Run(ctx, deps); globals become fields
      pool.go                    #   WANPool, slots, drain, swap
      select.go                  #   sched + affinity
      frontends.go               #   CONNECT + SOCKS listeners
      health.go                  #   canaries, passive health
    console/                     # control plane + SPA
      server.go                  #   one HTTP server: UI, /api/*, /metrics, /sub
      adapters.go                #   read module snapshots in-process
      static/                    #   embedded SPA
    platform/                    # no domain knowledge
      atomicfile/                #   tmp+rename, .bak
      ports/                     #   one allocator/validator
      xrayproc/                  #   child lifecycle, Pdeathsig
      logging/                   #   slog with component= field
  pkg/
    parser/                      # if kept public
    xraycfg/                     # if kept public
```

**Dependency rule:** `contract` and `platform` depend on nothing.
`catalog`, `qualify`, `proxy`, `console` depend on `contract`/settings/platform.
They **never import each other**; they exchange snapshots and narrow interfaces
defined in `contract`.

## Module contract

```go
type Module interface {
    Name() string
    Start(ctx context.Context) error
    Reload(s *settings.Snapshot) error // called on every accepted config change
    Stop(ctx context.Context) error
}
```

Optional capabilities:

```go
type HealthReporter interface{ Health() ModuleHealth }
type SnapshotProvider interface{ Snapshot() any } // console reads these
```

Rules:

- `Start` must return once the module is **ready or failed**, not block forever.
- `Reload` must be **transactional**: validate first; on error return without
  changing runtime state. The supervisor/manager keeps the old snapshot.
- `Stop` is idempotent and bounded by a context deadline.
- Panics are recovered at the supervisor boundary; a module crash does not kill
  the process.

## Ownership

| Module | Owns | Goroutines | Listeners |
|---|---|---|---|
| `catalog` | URLs, raw configs, state.json, working set | fetch/cycle, state saver | none (served by console) |
| `qualify` | test workers, port allocator, xray test children | bounded worker pool | none (temp loopback ports) |
| `proxy` | WAN pool, paths, selection, health, metrics | accept loops, canaries, replacer | CONNECT, SOCKS |
| `console` | HTTP API, SPA, auth | HTTP server | one control listener |
| `lifecycle` | start order, restart policy, shutdown | supervisor | none |
| `settings` | config file, snapshot, audit | file poll / SIGHUP | none |

One **control listener** serves UI, `/api/*`, `/metrics`, `/healthz`,
`/readyz`, and `/sub`. Data-plane listeners stay separate (CONNECT semantics
conflict with HTTP routes).

## One config

Replace `viberayd.env` + `viberoxy.env` + console env with **one file** and one
schema. Env vars override for containers.

```ini
# /etc/viber/viber.conf
server.listen          = 127.0.0.1:8090
proxy.proxy_port       = 1080
proxy.wan_count        = 4
proxy.wan_base_port    = 10700
proxy.test_base_port   = 10800
catalog.urls_file      = /etc/viber/urls.txt
catalog.interval       = 300s
catalog.depth          = standard
qualify.min_speed_mbps = 5.0
log.level              = info
```

Each field carries:

```go
type Field struct {
    Key      string   // canonical dotted key
    Aliases  []string // legacy env names: MINIMUM_SPEED, DAEMON_PARALLEL, ...
    Type     Type
    Default  string
    Min, Max *float64
    Enum     []string
    Secret   bool
    Apply    ApplyMode // live | next-cycle | rolling | restart
    Group    string    // proxy | catalog | qualify | console | log
}
```

The same slice drives parsing, validation, the UI schema, docs, and a test that
**every config field has exactly one schema entry and every entry is read**.
That test is the permanent guard against today's 23-key drift.

### Apply modes

| Mode | Meaning | Examples |
|---|---|---|
| `live` | atomic snapshot swap, next read uses it | log level, auth, route rules, thresholds |
| `next-cycle` | new value used when the next cycle/test starts | intervals, test sizes, depth |
| `rolling` | reconcile one unit at a time | `WAN_COUNT`, `XRAY_MUX`, WAN base port |
| `restart` | rejected for live apply | `LISTEN_ADDR`, state dir, xray binary |

**No partial apply:** validate the whole snapshot; any invalid field rejects the
update and keeps the previous generation. **No in-flight interruption:** a
connection captures the snapshot it needs at accept time; a cycle captures at
cycle start.

### Port validation (one map)

At startup and every apply, validate pairwise disjoint ranges:

```
control, CONNECT, SOCKS, metrics, WAN range (+spares), test range, catalog temp range
```

`WAN_BASE_PORT + WAN_COUNT + 2×WAN_COUNT` and
`TEST_BASE_PORT + maxTests + WAN_COUNT` must stay within 1..65535.
This removes the 10820–10823 collision by construction.

## Failure containment

- **Data plane first:** `proxy` starts before `catalog`/`console` and keeps
  serving if either fails.
- **Panic boundaries:** every module goroutine, HTTP handler, cycle worker, and
  connection handler recovers and reports; no `os.Exit`/`slog.Fatal` in
  libraries.
- **Module restart with backoff:** a broken catalog or console is restarted; the
  proxy is untouched.
- **Last-known-good:** invalid config, corrupt state, failed fetch, or disk error
  keeps the previous snapshot/state; writes are tmp+rename.
- **Readiness:** `/readyz` = at least one routable WAN. Console health never
  affects it.
- **Bounded memory:** compact catalog entries, cap, parse on demand for
  candidates; no duplicate parsed maps.
- **Bounded concurrency:** `qualify` workers and `MAX_XRAY_TESTS` are
  semaphore-bounded, separate from proxy goroutines.

## Migration path

| Phase | Deliverable | Risk |
|---|---|---|
| **0 (done)** | monorepo, all three histories, green build/tests | none |
| **1. Importable modules** | `Viberoxy` root leaves `package main`; each project exposes a small facade (`Run(ctx, cfg)`, `Reload`); `cmd/viber` starts Viberoy + Viberayd in one process | medium; mostly mechanical |
| **2. Native catalog path** | catalog feeds proxy directly; delete `SUBSCRIBER_URL` self-hop and `working.txt` transport; `/sub` from memory | medium |
| **3. One config** | settings schema/manager; legacy aliases; `/api/config`; live apply | medium |
| **4. Console in-process** | console reads snapshots instead of HTTP; delete supervisor | low |
| **5. One module, one parser** | collapse packages into one `viber` module; one parser/renderer/pipeline | high; do last |
| **6. Hot reload + packaging** | rolling WAN reconcile, listener swap, systemd/Docker, migration command | medium |

Phase 1 is the smallest slice that proves the thesis: **one PID serves CONNECT,
`/sub`, and the UI; killing the UI module does not stop the proxy.**

## Acceptance criteria

- One process, one PID, one log stream with `component=` fields.
- `CONNECT` and `/sub` work with the console module stopped.
- Changing every `live` field does not drop an in-flight CONNECT.
- Invalid config is rejected atomically with per-field errors.
- `WAN_COUNT` grow/shrink and `XRAY_MUX` changes roll without dropping tunnels.
- No port collisions by construction; startup prints the validated port map.
- `/readyz` never reports ready with zero routable WANs.
- `go test -race ./...` green, including a catalog-cycle + API-scrape race test.
