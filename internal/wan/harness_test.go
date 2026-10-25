package wan

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/config"
	"dissident/internal/contract"
)

const testUUID = "11111111-1111-1111-1111-111111111111"

// fakeProcess implements the manager's process surface without an xray binary.
type fakeProcess struct {
	port int
	pid  int

	mu        sync.Mutex
	stopped   bool
	stopCalls int
}

func (p *fakeProcess) Port() int { return p.port }
func (p *fakeProcess) PID() int  { return p.pid }
func (p *fakeProcess) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.stopped
}

func (p *fakeProcess) Stop(ctx context.Context) error {
	_ = ctx
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		p.stopCalls++
	}
	p.mu.Unlock()
	return nil
}

func (p *fakeProcess) isStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

func (p *fakeProcess) stops() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopCalls
}

// testResult is the default successful qualification result used by fakes.
func testResult(link contract.Link, score float64) contract.Result {
	return contract.Result{
		Hash:       link.Hash,
		Stage:      contract.StageReal,
		OK:         true,
		At:         time.Now().UTC(),
		TTFB:       10 * time.Millisecond,
		SpeedMbps:  score,
		ExitIP:     "203.0.113.9",
		TCPLatency: 5 * time.Millisecond,
	}
}

// fakeRuntime stands in for qualify.Tester + platform.StartXray. All function
// fields are optional; the defaults succeed immediately.
type fakeRuntime struct {
	mu sync.Mutex

	nextPort int
	testFn   func(context.Context, contract.Link) contract.Result
	promote  func(context.Context, contract.Link) (process, error)
	probeFn  func(context.Context, string) error

	testCalls    []string
	promoteCalls []string
	probeCalls   []string
	procs        []*fakeProcess
}

func (f *fakeRuntime) install(m *Manager) {
	m.testOne = f.test
	m.startPromoted = f.startPromoted
	m.startVerified = func(context.Context, contract.Link) (process, error) {
		return nil, errors.New("test: verification restore fallback disabled")
	}
	m.probe = f.probe
	m.stopProcess = func(ctx context.Context, p process) error { return p.Stop(ctx) }
}

func (f *fakeRuntime) setTestFn(fn func(context.Context, contract.Link) contract.Result) {
	f.mu.Lock()
	f.testFn = fn
	f.mu.Unlock()
}

func (f *fakeRuntime) setPromoteFn(fn func(context.Context, contract.Link) (process, error)) {
	f.mu.Lock()
	f.promote = fn
	f.mu.Unlock()
}

func (f *fakeRuntime) setProbeFn(fn func(context.Context, string) error) {
	f.mu.Lock()
	f.probeFn = fn
	f.mu.Unlock()
}

func (f *fakeRuntime) test(ctx context.Context, link contract.Link) contract.Result {
	f.mu.Lock()
	f.testCalls = append(f.testCalls, link.Hash)
	fn := f.testFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, link)
	}
	return testResult(link, 10)
}

func (f *fakeRuntime) startPromoted(ctx context.Context, link contract.Link) (process, error) {
	f.mu.Lock()
	f.promoteCalls = append(f.promoteCalls, link.Hash)
	fn := f.promote
	if fn == nil {
		f.nextPort++
		p := &fakeProcess{port: 20000 + f.nextPort, pid: 4000 + f.nextPort}
		f.procs = append(f.procs, p)
		f.mu.Unlock()
		return p, nil
	}
	f.mu.Unlock()
	return fn(ctx, link)
}

func (f *fakeRuntime) probe(ctx context.Context, addr string) error {
	f.mu.Lock()
	f.probeCalls = append(f.probeCalls, addr)
	fn := f.probeFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, addr)
	}
	return nil
}

func (f *fakeRuntime) counts() (tests, promotes, probes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.testCalls), len(f.promoteCalls), len(f.probeCalls)
}

func (f *fakeRuntime) promotionLinks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.promoteCalls))
	copy(out, f.promoteCalls)
	return out
}

func (f *fakeRuntime) testLinks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.testCalls))
	copy(out, f.testCalls)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testManager builds a manager with aggressive but deterministic timings and
// fake runtime hooks. Cleanup calls Stop.
func testManager(t *testing.T, mutate func(*config.Config)) (*Manager, *fakeRuntime, *catalog.Store) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.URLsFile = filepath.Join(dir, "urls.txt")
	cfg.Paths.StateFile = filepath.Join(dir, "state.json")
	cfg.Cycle.Interval = time.Hour
	cfg.Cycle.ActiveLiveness = time.Hour
	cfg.Cycle.ProbeWindow = 100 * time.Millisecond
	cfg.Cycle.ProbeBudgetPerMin = 600
	cfg.Cycle.ReadyTarget = 3
	cfg.Cycle.ReadyMin = 1
	cfg.Swap.DrainTimeout = 30 * time.Millisecond
	cfg.Swap.PromotionTimeout = time.Second
	cfg.Test.HTTPTimeout = 50 * time.Millisecond
	if mutate != nil {
		mutate(&cfg)
	}
	// An empty (but present) sources file keeps cycles deterministic; tests
	// that exercise fetch failure remove it explicitly.
	if err := os.MkdirAll(filepath.Dir(cfg.Paths.URLsFile), 0o755); err != nil {
		t.Fatalf("create urls parent: %v", err)
	}
	if err := os.WriteFile(cfg.Paths.URLsFile, nil, 0o600); err != nil {
		t.Fatalf("create empty urls file: %v", err)
	}
	manager, err := config.NewManager(cfg)
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	store := catalog.New()
	m := New(Options{
		Catalog:   store,
		Config:    manager,
		StateFile: cfg.Paths.StateFile,
		Logger:    discardLogger(),
	})
	fr := &fakeRuntime{}
	fr.install(m)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	return m, fr, store
}

func testLink(hash string) contract.Link {
	return contract.Link{
		Raw:      "vless://" + testUUID + "@example.com:443?type=tcp&security=none#" + hash,
		Hash:     hash,
		Protocol: contract.ProtocolVLESS,
		Name:     hash,
		Server:   "example.com",
		Port:     443,
		Params: map[string]string{
			"uuid":     testUUID,
			"type":     "tcp",
			"security": "none",
		},
	}
}

func testReady(hash string, score float64) contract.Ready {
	return contract.Ready{
		Link:      testLink(hash),
		Score:     score,
		LastCheck: time.Now().UTC(),
		ExitIP:    "203.0.113.9",
	}
}

// setActive installs a fake active WAN directly, bypassing promotion.
func setActive(t *testing.T, m *Manager, link contract.Link, port int) *fakeProcess {
	t.Helper()
	p := &fakeProcess{port: port, pid: 5000 + port}
	m.mu.Lock()
	m.active = &activeWAN{
		meta: contract.Active{
			Link:      link,
			Since:     time.Now().UTC(),
			ExitIP:    "203.0.113.9",
			XrayPID:   p.pid,
			LocalPort: port,
		},
		proc:    p,
		routeID: routeIDFor(link.Hash, p.pid),
	}
	m.mu.Unlock()
	return p
}

func setReady(m *Manager, ready ...contract.Ready) {
	m.mu.Lock()
	m.ready = cloneReady(ready)
	sortReady(m.ready)
	m.mu.Unlock()
}

func eventually(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitCtx(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func activeHash(m *Manager) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return ""
	}
	return m.active.meta.Link.Hash
}

func readyHashes(m *Manager) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.ready))
	for i := range m.ready {
		out[i] = m.ready[i].Link.Hash
	}
	return out
}

// errTest is a sentinel failure for Report/liveness tests.
var errTest = errors.New("test failure")
