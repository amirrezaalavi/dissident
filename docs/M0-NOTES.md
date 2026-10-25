# M0 notes — foundation

Status: **M0 complete** in `/home/netcon/dissident/viber`. The single root
Go module, shared contract, config, logging, lifecycle, control shell, CLI, and
example configs are in place. Catalog, share-link parsing, xray rendering,
qualification, and the real proxy were intentionally out of scope for this
milestone; they were delivered in M1–M6 (see `docs/M7-DEMO.md`).

## What was delivered

| File | Purpose |
|---|---|
| `go.mod` | `module dissident`, `go 1.26.0`, no external dependencies |
| `go.work` | Uses `.` plus the existing `viberoxy`, `viberayd`, `viber-console` entries |
| `Makefile` | Adds `dissident`, `dissident-test`, `dissident-vet`, `dissident-race`; legacy module targets still work |
| `.gitignore` | Adds `var/` for runtime state and logs |
| `cmd/dissident/main.go` | Flags, config load/check, signal handling, exit codes |
| `internal/contract/types.go` | Exactly the shared types from the spec |
| `internal/config/` | INI parser, defaults, validation, immutable snapshots, transactional manager |
| `internal/logging/` | slog system log, TSV access log, size rotation with gzip, retention |
| `internal/app/` | `Module`, ordered supervisor, panic isolation, app wiring |
| `internal/control/` | Server-rendered overview, `/healthz`, `/readyz`, `/metrics` |
| `configs/dissident.conf.example` | Exact config from the spec |
| `configs/urls.txt` | Harmless comment-only placeholder, no credentials |
| `docs/M0-NOTES.md` | This document |

All files are `gofmt`-clean and use only the standard library.

## Configuration

`config.Config` is nested by section: `Server`, `Paths`,
`Cycle`, `Swap`, `Test`, `URLs`, `Log`.
Canonical keys are the dotted spec keys, for example
`server.panel_listen`, `test.content_quorum`, `urls.speed`.

Parser rules:

- `[section]` headers and top-level dotted keys are both accepted.
- Keys and sections are case-insensitive and normalized to lowercase.
- Full-line `#` and `;` comments and blank lines are ignored.
- Values may be single- or double-quoted. Inline comments are not supported.
- Duplicate keys, unknown keys, malformed lines, and bad value types are errors
  with line numbers.
- URL lists are comma-separated.

`config.Load(path)` starts from `Defaults()`, applies the file, and
calls `Validate()`. A failed load returns a zero `Config`; a bad
file can never become a runtime snapshot.

Validation covers:

- listen addresses (`host:port`, port 0-65535; port 0 is allowed for
  ephemeral/test listeners) and distinct panel/proxy addresses;
- non-empty `paths.*`, including `xray_bin`;
- positive durations, worker counts, sizes, and thresholds;
- `ready_min <= ready_target`;
- `content_quorum <= len(urls.content)`;
- HTTP(S) URLs with a host (`probe`, `verify`, `content`
  required; `speed`, fallback, and exit-IP may be empty);
- log level, retention, and size.

Snapshots and manager:

```
type Snapshot struct {
    Generation uint64
    Config     Config
}
```

- `NewManager(cfg)` validates and publishes generation 1.
- `Manager.Current() *Snapshot` is lock-free and never nil for a live
  manager. Treat it as read-only.
- `Manager.Apply(cfg) error` validates first, then publishes
  `generation+1` atomically. Any error leaves the old pointer and
  generation untouched.
- `Config.Clone()` and `Snapshot.Clone()` deep-copy URL slices.

## Interfaces later agents must use

### Module and supervisor

```
type Module interface {
    Name() string
    Start(ctx context.Context) error
    Reload(s *config.Snapshot) error
    Stop(ctx context.Context) error
}
```

Optional capabilities in `internal/app`:

- `ReloadValidator{ ValidateReload(*config.Snapshot) error }`
- `HealthReporter{ Health() Health }`
- `SnapshotProvider{ Snapshot() any }`

Supervisor behavior:

- `NewSupervisor(SupervisorOptions{Logger, StopTimeout})`; default stop
  timeout is 30s.
- `Add(module)` before `Start`; names must be unique.
- `Start(ctx, snapshot)` starts in registration order. On failure it
  stops already-started modules in reverse order, cancels the supervisor
  context, and returns a joined error. A failed supervisor cannot be restarted.
- `Reload(snapshot)` is two-phase: call `ValidateReload` on every
  module that implements it, then call `Reload` in order. If a module
  fails, previously reloaded modules are rolled back to the previous snapshot.
  The supervisor's current snapshot only advances on full success. Rollback is
  best-effort for the module that failed.
- `Stop(ctx)` cancels tracked goroutines, stops in reverse order, waits
  for tracked goroutines until the ctx deadline or `StopTimeout`, and is
  idempotent.
- `Go(name, func(context.Context)) bool` runs a tracked goroutine with
  panic recovery. It returns false when not running or already stopping.
- Panics in Start/Reload/Stop/ValidateReload and tracked goroutines are
  recovered, logged with a stack, and counted by `RecoveredPanics()`.

Do not call supervisor lifecycle methods from a module callback that the
supervisor is currently invoking; the supervisor serializes start/reload/stop.
`Go` and read-only accessors are safe.

### App wiring

`app.New(app.Options{...})` validates the config, creates logging, the
config manager, the supervisor, and the control module. `Options.Snapshot
func() contract.Snapshot` is the runtime snapshot provider; nil means an
empty view that still carries the config generation and a current timestamp.
`Options.ControlListen` exists for tests/embedding and overrides
`server.panel_listen`; production should leave it empty.

- `Start(ctx)` starts the supervisor/control listener and succeeds with
  no active WAN. `/readyz` then returns 503.
- `Stop(ctx)` stops modules and closes logs; idempotent.
- `Run(ctx)` starts, waits for cancellation, then stops with the config's
  `swap.drain_timeout`.
- `ReloadConfig()` reloads `Options.ConfigPath` and applies it.
- `ApplyConfig(cfg)` validates, reloads modules transactionally, then
  publishes the next generation. Rejected config leaves modules and the manager
  on the old generation. The log level is applied live; `log_dir` and
  `log.access` changes only warn and need a restart.
- `Snapshot()` merges the supplied provider with the config generation.
- `Config()`, `Logger()`, `ControlAddr()`, and
  `ConfigPath()` are read-only accessors.

### Control plane

Constructed with `control.New(control.Options{Listen, Snapshot, Logger,
Version, timeouts...})`. Routes:

| Route | Behavior |
|---|---|
| `GET /` | Server-rendered overview using `html/template`; no SPA and no JSON control API |
| `GET /healthz` | 200 `ok` whenever the process serves |
| `GET /readyz` | 200 `ready` only when `Snapshot.Active != nil`; otherwise 503 with a clear reason |
| `GET /metrics` | Prometheus text format |

Metric names: `dissident_up`, `dissident_config_generation`,
`dissident_active_wan`, `dissident_ready_entries`,
`dissident_catalog_entries`, `dissident_supported_entries`,
`dissident_testing_entries`.

HTTP server timeouts: read-header 5s, read 15s, write 30s, idle 60s (defaults).
Graceful shutdown uses `http.Server.Shutdown(ctx)`. Handler panics are
recovered and returned as 500. `Reload` rejects a changed
`panel_listen` with an error so the config manager keeps the previous
snapshot; rebinding requires a process restart.

### Logging and access format

`logging.New(logging.Options{Dir, Level, Access, RetentionDays, MaxSizeMB,
Stderr})` returns `System() *slog.Logger`, `Access(record) error`,
`SetLevel(name) error`, `Level()`, and `Close()`.

- System log: slog text to stderr and `Dir/dissident.log`.
- Access log: `Dir/access.log`, tab-separated, one line per record.

Base columns:

```
time  src  state  dst  domain:port
```

When any extended field is present, all six are appended:

```
proto  wan  up_bytes  down_bytes  duration_ms  reason
```

Time is UTC with millisecond precision, `2006-01-02T15:04:05.000Z07:00`.
Empty strings render as `-`; durations are whole milliseconds; counters
are decimal bytes. Tabs, newlines, and carriage returns in values are replaced
with `_`; URL userinfo and raw `user:pass@` prefixes are stripped,
so credentials cannot reach the access log. Writes are serialized and a whole
line is written under one lock.

Rotation (both files):

- Rotate before a write that would exceed `max_size_mb`.
- Gzip the old file to
  `<name>.<UTC timestamp>.gz`, for example
  `dissident.log.20261025T130000.000000000Z.gz`.
- Remove archives older than `retention_days` and keep at most 50 by
  default (`RotationOptions.MaxBackups`; negative means no cap).
- Cleanup runs at open and after each rotation.
- `internal/logging` also exports `OpenRotating`,
  `RotatingFile`, `FormatAccessRecord`, and `ParseLevel`
  for tests and later modules.

### CLI and exit codes

`dissident [-config path] [-check-config] [-version]`

- `-config` defaults to `./configs/dissident.conf`.
- `-check-config` loads and validates, prints `config OK: path`,
  and exits without opening logs or binding sockets.
- `-version` prints `dissident <version>` (`dev` unless set
  with `-ldflags "-X main.version=..."`).
- Signals: SIGINT and SIGTERM run graceful shutdown; SIGHUP reloads the config
  file and keeps the old snapshot if reload fails.

Exit codes: 0 success, 1 runtime/startup failure, 2 usage/flag error,
3 config load/validation error.

## Verification performed

Environment: Go 1.26.6 linux/amd64. The default Go build cache is read-only in
this workspace, so commands must use the workspace cache:

```sh
cd /home/netcon/dissident/viber
export GOCACHE=/home/netcon/dissident/.cache/go-build
export GOTMPDIR=/home/netcon/dissident/.cache/tmp

go build ./...
go vet ./...
go test ./... -count=1
```

Result at the end of M0: all three commands passed. Targeted tests:

```sh
go test ./internal/config ./internal/logging ./internal/app ./internal/control -count=1
go test -race ./internal/config ./internal/logging ./internal/app ./internal/control -count=1
```

Make targets (honor an exported `GOCACHE`/`GOTMPDIR`):

```sh
make dissident          # -> bin/dissident
make dissident-test
make dissident-vet
make dissident-race
make build              # dissident + legacy module builds
make check              # build + vet + test
```

CLI smoke checks:

```sh
./bin/dissident -version
./bin/dissident -check-config -config ./configs/dissident.conf.example
```

Legacy `viberoxy`, `viberayd`, and `viber-console` files
were not edited and their test suites were not run.

## Known gaps and next steps

- Catalog fetch/import, share-link parsing, xray rendering/preflight,
  qualification, state persistence, and the real proxy are not in M0. They are
  the next packages under `internal/`.
- The proxy listen address is validated but nothing binds it yet. The panel
  serves with no active WAN and `/readyz` stays 503 until a module
  publishes `Snapshot.Active != nil`.
- The control plane has only the overview shell. Logs, SSE, config editing,
  and additional pages are for later milestones; there is no JSON control API.
- No auth or TLS for the panel (demo decision).
- `panel_listen` changes require restart. `log_dir` and
  `log.access` changes are not hot-applied. Control HTTP timeout changes
  are not applied live.
- `xray_bin` is checked for non-emptiness only; existence/executability
  belongs to the qualification/xray milestone.
- Port collision validation is limited to panel vs proxy; WAN/test port ranges
  do not exist yet.
- No environment-variable overrides, includes, or legacy-key aliases; the spec's
  one INI file is the only source.
- `go test ./...` at the root also sees packages added by other agents
  (for example `internal/uri`, `internal/xraycfg`,
  `internal/proxy`); their state can change independently of M0.
