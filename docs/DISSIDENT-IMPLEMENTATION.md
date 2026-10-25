# Dissident implementation spec (demo)

Status: authoritative for the demo build. Managed by the lead; implemented by subagents.

## Product

`dissident` is one Go module, one binary, one process:

- catalog: subscription fetch/import, parse/support gate, dedupe
- qualify: strict staged tests, one full pipeline, no quick/standard/full modes
- proxy: one active WAN + ready standby list, HTTP CONNECT and SOCKS5 on one port
- control: server-rendered panel, logs, metrics
- config: one INI-style file, atomic snapshots, SIGHUP
- state: persisted active and ready list for fast restart

No HTTP between modules. No `/sub`. No raw control API. No supervisor process.

## Locked demo decisions

| Area | Decision |
|---|---|
| Module | `module dissident` at repo root |
| CLI | `dissident -config ./configs/dissident.conf` |
| Proxy listen | `0.0.0.0:10909` (configurable), first-byte sniff |
| Panel listen | `0.0.0.0:8090` (configurable), server-rendered |
| Active WAN | exactly 1 |
| Ready | target 5, min 3 |
| Replace | the only manual WAN action |
| Cycle | 1800s |
| Active liveness | 15s, 3 probes / 8s |
| Ready check | 100s, TCP/TLS only |
| Test order | parse -> dedupe -> TCP -> TLS -> protocol -> render -> real HTTP -> content -> speed -> stability |
| Cheap workers | 8, real workers 1 |
| Content quorum | 3 of 4, minimum 4096 bytes |
| Speed | read exactly 4,000,000 bytes |
| Auth/TLS | none for demo |
| Dependencies | standard library only |

Domains:

- liveness: `https://www.gstatic.com/generate_204`, `https://www.cloudflare.com/cdn-cgi/trace`
- verify: `https://www.google.com/generate_204`, `https://1.1.1.1/cdn-cgi/trace`
- content: `https://youtube.com`, `https://www.cloudflare.com`, `https://telegram.org/`, `https://www.instagram.com/`
- speed: `https://proof.ovh.net/files/10Mb.dat`, fallback `https://speed.cloudflare.com/__down?bytes=`
- exit IP: `https://api.ipify.org/` (optional, not on every liveness check)

Supported v1 parser/renderer matrix: VLESS tcp/ws/grpc/reality/xhttp, VMess tcp/ws/grpc, Trojan tcp/ws/grpc, Shadowsocks without plugin, SOCKS5. Reject TUIC/Hysteria2/WireGuard/plugins/unknown transports with a clear reason.

## Layout

```
go.mod
cmd/dissident/main.go
internal/
  app/          lifecycle, module supervisor, wiring
  config/       schema, INI load, validate, snapshot
  contract/     shared data types only
  catalog/      fetch/import, parse gate, dedupe, state
  uri/          share-link parse into contract.Link
  xraycfg/      raw-faithful xray JSON renderer + preflight
  qualify/      staged pipeline, domains, speed
  proxy/        listeners, CONNECT/SOCKS5, route provider
  control/      server-rendered panel, SSE, logs
  logging/      system log + TSV access log, rotation
  platform/     atomic files, ephemeral ports, xray child processes
web/templates/
configs/dissident.conf.example
```

## Shared contract (package `dissident/internal/contract`)

```go
type Protocol string

const (
    ProtocolVLESS       Protocol = "vless"
    ProtocolVMess       Protocol = "vmess"
    ProtocolTrojan      Protocol = "trojan"
    ProtocolShadowsocks Protocol = "ss"
    ProtocolSOCKS5      Protocol = "socks5"
)

type Link struct {
    Raw      string
    Hash     string
    Protocol Protocol
    Name     string
    Server   string
    Port     int
    Params   map[string]string
}

type EntryState string

const (
    StateCatalog     EntryState = "catalog"
    StateTesting     EntryState = "testing"
    StateReady       EntryState = "ready"
    StateActive      EntryState = "active"
    StateFailed      EntryState = "failed"
    StateUnsupported EntryState = "unsupported"
)

type Stage string

const (
    StageParse       Stage = "parse"
    StageDedupe      Stage = "dedupe"
    StageTCP         Stage = "tcp"
    StageTLS         Stage = "tls"
    StageProtocol    Stage = "protocol"
    StageRender      Stage = "render"
    StageReal        Stage = "real"
    StageContent     Stage = "content"
    StageSpeed       Stage = "speed"
    StageStability   Stage = "stability"
)

type Result struct {
    Hash       string
    Stage      Stage
    OK         bool
    Reason     string
    At         time.Time
    TCPLatency time.Duration
    TTFB       time.Duration
    SpeedMbps  float64
    HTTPStatus int
    Bytes      int64
    ExitIP     string
}

type Entry struct {
    Link     Link
    State    EntryState
    Score    float64
    Last     Result
    Attempts int
    Updated  time.Time
}

type Ready struct {
    Link      Link
    Score     float64
    LastCheck time.Time
    ExitIP    string
}

type Active struct {
    Link      Link
    Since     time.Time
    ExitIP    string
    XrayPID   int
    LocalPort int
}

type Snapshot struct {
    Generation     uint64
    Active         *Active
    Ready          []Ready
    CatalogCount   int
    SupportedCount int
    Testing        int
    Updated        time.Time
}

type AccessRecord struct {
    Time     time.Time
    Src      string
    State    string
    Dst      string
    Domain   string
    Port     int
    Proto    string
    WAN      string
    Up       int64
    Down     int64
    Duration time.Duration
    Reason   string
}
```

## Package APIs (must exist for integration)

```go
package uri

func Parse(raw string) (contract.Link, error)
func ParseMany(data []byte) ([]contract.Link, []error)
```

```go
package xraycfg

func Supports(link contract.Link) error
func Render(link contract.Link, localPort int) ([]byte, error)
func Preflight(ctx context.Context, xrayBin string, cfg []byte) error
```

```go
package proxy

type Dialer interface {
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type Route struct {
    ID   string
    Dial Dialer
}

type RouteProvider interface {
    Route(ctx context.Context) (Route, error)
    Report(ctx context.Context, route Route, err error)
}

type Options struct {
    Listen   string
    Provider RouteProvider
    Access   func(contract.AccessRecord)
    Logger   *slog.Logger
}

type Service struct { /* ... */ }

func New(opts Options) *Service
func (s *Service) Start(ctx context.Context) error
func (s *Service) Stop(ctx context.Context) error
```

## Config example

```ini
[server]
panel_listen = 0.0.0.0:8090
proxy_listen = 0.0.0.0:10909

[paths]
urls_file  = ./configs/urls.txt
state_file = ./var/state.json
log_dir    = ./var/log
xray_bin   = /usr/local/bin/xray

[cycle]
interval             = 1800s
active_liveness      = 15s
active_idle          = 60s
ready_liveness       = 100s
probe_count          = 3
probe_window         = 8s
probe_budget_per_min = 60
ready_min            = 3
ready_target         = 5

[swap]
drain_timeout     = 30s
promotion_timeout = 15s

[test]
cheap_workers     = 8
real_workers      = 1
real_test_cap     = 0
tcp_timeout       = 3s
tls_timeout       = 3s
protocol_timeout  = 3s
xray_timeout      = 10s
http_timeout      = 5s
speed_size        = 4000000
min_speed_mbps    = 5
max_ttfb_ms       = 2500
stability_passes  = 2
content_quorum    = 3
content_min_bytes = 4096

[urls]
probe          = https://www.gstatic.com/generate_204,https://www.cloudflare.com/cdn-cgi/trace
verify         = https://www.google.com/generate_204,https://1.1.1.1/cdn-cgi/trace
content        = https://youtube.com,https://www.cloudflare.com,https://telegram.org/,https://www.instagram.com/
speed          = https://proof.ovh.net/files/10Mb.dat
speed_fallback = https://speed.cloudflare.com/__down?bytes=
exit_ip        = https://api.ipify.org/

[log]
level          = info
access         = true
retention_days = 30
max_size_mb    = 100
```

Unknown keys are errors. Invalid config never replaces the previous runtime snapshot.

## Engineering rules

- Go 1.26, standard library only.
- `gofmt` clean, `go vet ./...` clean, `go test ./... -count=1` green.
- No network calls in unit tests. Use httptest and fake dialers.
- Every goroutine bounded and cancelable.
- Access logs must never contain credentials.
- Do not run the upstream viberoxy/viberayd/viber-console test suites.
- Do not edit files outside your assigned package set.

## Integration APIs (M2/M3/M5)

These are the contracts between the next subagents. Implement them exactly; do not
change the foundation packages' public API without telling the lead.

### Optional egress proxy for restricted demo hosts

The demo host reaches the internet only through an HTTP proxy. Production deployments
leave this empty and xray dials directly. Add `test.egress_proxy` (optional URL,
http/https scheme) to config; example stays commented out. When set:

- TCP/TLS stages dial servers through HTTP CONNECT.
- every rendered xray outbound gets an HTTP outbound tagged `egress` and
  `streamSettings.sockopt.dialerProxy = "egress"`.
- this is a demo/network escape hatch, not a routing feature. Never fall back to
  direct egress for proxied traffic.

```go
package platform

type Xray struct {
    Port       int
    ConfigPath string
    Exited     <-chan struct{}
}
func StartXray(ctx context.Context, bin string, cfg []byte, dir string) (*Xray, error)
func (x *Xray) Stop(ctx context.Context) error
func (x *Xray) Alive() bool
func (x *Xray) PID() int
func FreePort() (int, error)

type SOCKS5Dialer struct {
    Addr    string
    Timeout time.Duration
}
func (d *SOCKS5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error)

func RenderWithEgress(link contract.Link, port int, egressProxy string) ([]byte, error)
func NewEgressDialer(proxyURL string, timeout time.Duration) (func(context.Context, string, string) (net.Conn, error), error)
```

```go
package catalog

type Store struct { /* ... */ }
func New() *Store
func (s *Store) Merge(links []contract.Link) int
func (s *Store) Entries() []contract.Entry
func (s *Store) Pending(limit int) []contract.Entry
func (s *Store) Mark(hash string, state contract.EntryState, res contract.Result)
func (s *Store) Stats() (total, supported, testing int)

func LoadSources(ctx context.Context, path string, timeout time.Duration) ([]contract.Link, []error, error)
func ParseSources(data []byte) ([]contract.Link, []error)
```

`LoadSources` accepts a file containing share links and/or http(s) subscription URLs,
one per line. Fetch at most 8 MiB per source, follow redirects with a timeout, and parse
base64 payloads. Missing file is an error; a failed source is reported in `[]error` but
does not discard good sources.

```go
package qualify

type Options struct {
    Test        config.TestConfig
    URLs        config.URLsConfig
    XrayBin     string
    EgressProxy string
    Logger      *slog.Logger
    TempDir     string
}

type Tester struct { /* ... */ }
func New(opts Options) *Tester
func (t *Tester) Test(ctx context.Context, link contract.Link) contract.Result
func (t *Tester) Promote(ctx context.Context, link contract.Link) (contract.Result, *platform.Xray, error)
func (t *Tester) Verify(ctx context.Context, socksAddr string) error
func (t *Tester) Speed(ctx context.Context, socksAddr string) (mbps float64, ttfb time.Duration, err error)
func (t *Tester) Probe(ctx context.Context, socksAddr string) error
```

Test order is strictly: support -> TCP -> TLS (TLS links only; skip reality) -> protocol ->
render -> preflight -> real HTTP -> content quorum -> speed/TTFB -> stability. A config
that has not passed TCP/TLS/protocol must never start the real xray. The real xray is
started once per candidate and reused for real/content/speed/stability, then stopped.
`Promote` starts a candidate xray, verifies a 2xx through it, optionally measures speed,
and returns the running process; on failure it stops the process and returns the error.
`Verify` and `Probe` follow redirects and require a 2xx. `Speed` reads exactly
`test.speed_size` bytes, tries `urls.speed` then `urls.speed_fallback` on transport
errors or non-2xx, and returns Mbps = bytes*8/elapsed/1e6.

```go
package wan

type Options struct {
    Catalog   *catalog.Store
    Tester    *qualify.Tester
    Config    *config.Manager
    StateFile string
    Logger    *slog.Logger
}

type Manager struct { /* ... */ }
func New(opts Options) *Manager
func (m *Manager) Start(ctx context.Context) error
func (m *Manager) Stop(ctx context.Context) error
func (m *Manager) Snapshot() contract.Snapshot
func (m *Manager) Entries() []contract.Entry
func (m *Manager) Route(ctx context.Context) (proxy.Route, error)
func (m *Manager) Report(ctx context.Context, route proxy.Route, err error)
func (m *Manager) RunCycle(ctx context.Context) error
func (m *Manager) Replace(ctx context.Context) error
func (m *Manager) RemoveReady(hash string) error
func (m *Manager) Cycling() bool
func (m *Manager) LastCycle() time.Time
func (m *Manager) NextCycle() time.Time
func (m *Manager) LastError() string
```

Manager invariants:
- exactly one active xray process; ready entries are metadata only
- Route returns the current active process's SOCKS5 dialer (implements proxy.Dialer)
- replacement is make-before-break: start/verify candidate, atomically swap, keep the old
  process alive for `swap.drain_timeout` so in-flight connections finish, then stop it
- three passive failures for the active route within `cycle.probe_window` trigger one
  asynchronous Replace; failure reports must not block the proxy
- liveness probes every `cycle.active_liveness`; 3 failed probes in the window replace
- persistence is atomic (0600) and stores active + ready raw links and metadata; on
  startup, reload the active process if possible, else run a cycle
- a cycle tests catalog pending entries in TCP-latency order, stops at ready_target, and
  promotes the first passer when there is no active
- no direct-egress fallback, no second active

```go
package control

type Data interface {
    Snapshot() contract.Snapshot
    Entries() []contract.Entry
    Cycling() bool
    LastCycle() time.Time
    NextCycle() time.Time
    LastError() string
    Replace(context.Context) error
    RunCycle(context.Context) error
    CancelCycle()
    RemoveReady(string) error
    Subscriptions() []string
    AddSubscription(string) error
    RemoveSubscription(string) error
}

type Options struct {
    Listen     string
    Data       Data
    Config     *config.Manager
    AccessLog  string
    SystemLog  string
    Logger     *slog.Logger
}

type Server struct { /* ... */ }
func New(opts Options) *Server
func (s *Server) Start(ctx context.Context) error
func (s *Server) Stop(ctx context.Context) error
```

Control pages are server-rendered HTML, no JSON control API: Overview, WAN, Tests,
Subscriptions, Settings, Logs. Only WAN actions are Replace and Remove-ready. No drain or
force-stop buttons. `/metrics` is Prometheus text on the panel listener. Forms POST and
redirect (PRG). Every handler recovers its own panic.
