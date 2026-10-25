package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeModule struct {
	name string
	log  *[]string

	startErr    error
	reloadErr   error
	stopErr     error
	validateErr error

	panicStart    bool
	panicReload   bool
	panicStop     bool
	panicValidate bool

	starts  int
	stops   int
	reloads []uint64
}

func (f *fakeModule) Name() string { return f.name }

func (f *fakeModule) record(entry string) {
	if f.log != nil {
		*f.log = append(*f.log, entry)
	}
}

func (f *fakeModule) Start(ctx context.Context) error {
	f.starts++
	f.record("start:" + f.name)
	if f.panicStart {
		panic("start panic in " + f.name)
	}
	return f.startErr
}

func (f *fakeModule) Reload(s *config.Snapshot) error {
	if s != nil {
		f.reloads = append(f.reloads, s.Generation)
		f.record("reload:" + f.name + ":" + itoa(s.Generation))
	}
	if f.panicReload {
		panic("reload panic in " + f.name)
	}
	return f.reloadErr
}

func (f *fakeModule) ValidateReload(s *config.Snapshot) error {
	if f.panicValidate {
		panic("validate panic in " + f.name)
	}
	return f.validateErr
}

func (f *fakeModule) Stop(ctx context.Context) error {
	f.stops++
	f.record("stop:" + f.name)
	if f.panicStop {
		panic("stop panic in " + f.name)
	}
	return f.stopErr
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func snap(generation uint64) *config.Snapshot {
	return &config.Snapshot{Generation: generation, Config: config.Defaults()}
}

func newTestSupervisor(modules ...Module) *Supervisor {
	sup := NewSupervisor(SupervisorOptions{Logger: silentLogger(), StopTimeout: time.Second})
	for _, m := range modules {
		if err := sup.Add(m); err != nil {
			panic(err)
		}
	}
	return sup
}

func TestSupervisorLifecycle(t *testing.T) {
	var log []string
	a := &fakeModule{name: "a", log: &log}
	b := &fakeModule{name: "b", log: &log}
	c := &fakeModule{name: "c", log: &log}
	sup := newTestSupervisor(a, b, c)

	if err := sup.Start(context.Background(), snap(1)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := strings.Join(log, ","); got != "start:a,start:b,start:c" {
		t.Fatalf("start order = %s", got)
	}
	if sup.Current() == nil || sup.Current().Generation != 1 {
		t.Fatalf("current snapshot = %+v", sup.Current())
	}

	if err := sup.Reload(snap(2)); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := strings.Join(log, ","); !strings.HasSuffix(got, "reload:a:2,reload:b:2,reload:c:2") {
		t.Fatalf("reload order = %s", got)
	}
	if sup.Current().Generation != 2 {
		t.Fatalf("generation = %d", sup.Current().Generation)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := strings.Join(log, ","); !strings.HasSuffix(got, "stop:c,stop:b,stop:a") {
		t.Fatalf("stop order = %s", got)
	}
	if err := sup.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if sup.Go("late", func(context.Context) {}) {
		t.Fatal("Go started work after Stop")
	}
}

func TestSupervisorStartFailureStopsStarted(t *testing.T) {
	var log []string
	startErr := errors.New("b cannot start")
	a := &fakeModule{name: "a", log: &log}
	b := &fakeModule{name: "b", log: &log, startErr: startErr}
	c := &fakeModule{name: "c", log: &log}
	sup := newTestSupervisor(a, b, c)

	err := sup.Start(context.Background(), snap(1))
	if !errors.Is(err, startErr) {
		t.Fatalf("Start error = %v, want wrapped %v", err, startErr)
	}
	got := strings.Join(log, ",")
	if got != "start:a,start:b,stop:a" {
		t.Fatalf("call order = %s", got)
	}
	if c.starts != 0 {
		t.Fatal("module c started after module b failed")
	}
	if err := sup.Start(context.Background(), snap(1)); err == nil {
		t.Fatal("Start after a failed Start should fail")
	}
}

func TestSupervisorReloadRollsBack(t *testing.T) {
	var log []string
	a := &fakeModule{name: "a", log: &log}
	reloadErr := errors.New("b rejects reload")
	b := &fakeModule{name: "b", log: &log, reloadErr: reloadErr}
	sup := newTestSupervisor(a, b)

	if err := sup.Start(context.Background(), snap(1)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := sup.Reload(snap(2))
	if !errors.Is(err, reloadErr) {
		t.Fatalf("Reload error = %v, want wrapped %v", err, reloadErr)
	}
	if sup.Current().Generation != 1 {
		t.Fatalf("generation changed to %d after failed reload", sup.Current().Generation)
	}
	if len(a.reloads) != 2 || a.reloads[0] != 2 || a.reloads[1] != 1 {
		t.Fatalf("module a reloads = %v, want [2 1]", a.reloads)
	}
	if len(b.reloads) != 1 || b.reloads[0] != 2 {
		t.Fatalf("module b reloads = %v, want [2]", b.reloads)
	}
}

func TestSupervisorReloadValidationPreventsChanges(t *testing.T) {
	var log []string
	validationErr := errors.New("bad new config")
	a := &fakeModule{name: "a", log: &log}
	b := &fakeModule{name: "b", log: &log, validateErr: validationErr}
	sup := newTestSupervisor(a, b)

	if err := sup.Start(context.Background(), snap(1)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := sup.Reload(snap(2))
	if !errors.Is(err, validationErr) {
		t.Fatalf("Reload error = %v, want wrapped %v", err, validationErr)
	}
	if len(a.reloads) != 0 || len(b.reloads) != 0 {
		t.Fatalf("modules were reloaded despite validation failure: %v %v", a.reloads, b.reloads)
	}
	if sup.Current().Generation != 1 {
		t.Fatalf("generation = %d, want 1", sup.Current().Generation)
	}
}

func TestSupervisorPanicIsolation(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		var log []string
		a := &fakeModule{name: "a", log: &log}
		b := &fakeModule{name: "b", log: &log, panicStart: true}
		sup := newTestSupervisor(a, b)
		err := sup.Start(context.Background(), snap(1))
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Start error = %v", err)
		}
		if a.stops != 1 {
			t.Fatalf("module a stops = %d, want 1", a.stops)
		}
		if sup.RecoveredPanics() != 1 {
			t.Fatalf("recovered panics = %d", sup.RecoveredPanics())
		}
	})

	t.Run("reload", func(t *testing.T) {
		m := &fakeModule{name: "m", panicReload: true}
		sup := newTestSupervisor(m)
		if err := sup.Start(context.Background(), snap(1)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		err := sup.Reload(snap(2))
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Reload error = %v", err)
		}
		if sup.RecoveredPanics() != 1 {
			t.Fatalf("recovered panics = %d", sup.RecoveredPanics())
		}
		if err := sup.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		m := &fakeModule{name: "m", panicStop: true}
		sup := newTestSupervisor(m)
		if err := sup.Start(context.Background(), snap(1)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		err := sup.Stop(context.Background())
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Stop error = %v", err)
		}
	})

	t.Run("goroutine", func(t *testing.T) {
		m := &fakeModule{name: "m"}
		sup := newTestSupervisor(m)
		if err := sup.Start(context.Background(), snap(1)); err != nil {
			t.Fatalf("Start: %v", err)
		}
		ran := make(chan struct{})
		if !sup.Go("worker", func(context.Context) {
			close(ran)
			panic("goroutine boom")
		}) {
			t.Fatal("Go refused to start")
		}
		<-ran
		if err := sup.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if sup.RecoveredPanics() != 1 {
			t.Fatalf("recovered panics = %d", sup.RecoveredPanics())
		}
	})
}

type blockingStopModule struct {
	name string
}

func (b *blockingStopModule) Name() string                  { return b.name }
func (b *blockingStopModule) Start(context.Context) error   { return nil }
func (b *blockingStopModule) Reload(*config.Snapshot) error { return nil }
func (b *blockingStopModule) Stop(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestSupervisorStopHonorsDeadline(t *testing.T) {
	sup := newTestSupervisor(&blockingStopModule{name: "blocking"})
	if err := sup.Start(context.Background(), snap(1)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := sup.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("Stop error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop took %v, deadline was not honored", elapsed)
	}
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.PanelListen = "127.0.0.1:0"
	// config.Validate rejects identical listen strings ("...:0" included),
	// so the proxy uses the equivalent loopback hostname with the same
	// ephemeral port. Both bind on 127.0.0.1.
	cfg.Server.ProxyListen = "localhost:0"
	dir := t.TempDir()
	cfg.Paths.LogDir = filepath.Join(dir, "log")
	cfg.Paths.StateFile = filepath.Join(dir, "state.json")
	cfg.Paths.URLsFile = filepath.Join(dir, "urls.txt")
	cfg.Paths.XrayBin = "/bin/true"
	// Hermetic startup cycle: the source file contains no links, so no
	// candidate is ever dialed and no external network is needed. The long
	// interval keeps periodic cycles out of the test window.
	if err := os.WriteFile(cfg.Paths.URLsFile, []byte("# no subscriptions in tests\n"), 0o600); err != nil {
		t.Fatalf("write urls file: %v", err)
	}
	cfg.Cycle.Interval = time.Hour
	return cfg
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

func dialSucceeds(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = conn.Close()
}

func dialFails(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("dial %s succeeded after Stop; listener is still open", addr)
	}
}

// waitForNoModuleGoroutines waits for goroutines owned by the wired modules to
// exit. A leaked accept/reload/cycle goroutine keeps its package in the stack.
func waitForNoModuleGoroutines(t *testing.T) {
	t.Helper()
	markers := []string{
		"dissident/internal/control",
		"dissident/internal/proxy",
		"dissident/internal/wan",
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		stack := string(buf[:n])
		leaked := ""
		for _, marker := range markers {
			if strings.Contains(stack, marker) {
				leaked = marker
				break
			}
		}
		if leaked == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine from %s is still running after Stop\n%s", leaked, stack)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAppServesWithoutActiveWAN(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(Options{Version: "test", Config: cfg, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Stop(context.Background())

	if got, want := strings.Join(application.sup.Modules(), ","), "control,proxy,wan"; got != want {
		t.Fatalf("module order = %q, want %q", got, want)
	}

	addr := application.ControlAddr()
	if addr == "" {
		t.Fatal("ControlAddr is empty after Start")
	}
	proxyAddr := application.ProxyAddr()
	if proxyAddr == "" {
		t.Fatal("ProxyAddr is empty after Start")
	}
	dialSucceeds(t, proxyAddr)
	base := "http://" + addr

	if code, body := httpGet(t, base+"/healthz"); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	if code, body := httpGet(t, base+"/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "no active WAN") {
		t.Fatalf("readyz = %d %q", code, body)
	}
	if code, body := httpGet(t, base+"/"); code != http.StatusOK || !strings.Contains(body, "No active WAN") {
		t.Fatalf("overview = %d, body lacks readiness warning", code)
	}
	if code, body := httpGet(t, base+"/wan"); code != http.StatusOK || !strings.Contains(body, "disabled>Replace active") {
		t.Fatalf("wan = %d, Replace should be disabled with an empty ready list", code)
	}
	if code, body := httpGet(t, base+"/tests"); code != http.StatusOK || !strings.Contains(body, "Run cycle") {
		t.Fatalf("tests = %d, cycle action is missing", code)
	}
	if code, body := httpGet(t, base+"/metrics"); code != http.StatusOK || !strings.Contains(body, "dissident_active_wan 0") {
		t.Fatalf("metrics = %d %q", code, body)
	}
	got := application.Snapshot()
	if got.Active != nil || got.Generation != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	if len(got.Ready) != 0 {
		t.Fatalf("ready = %d, want 0", len(got.Ready))
	}
}

func TestAppStopIdempotentAndClosesListeners(t *testing.T) {
	before := runtime.NumGoroutine()
	cfg := testConfig(t)
	application, err := New(Options{Version: "test", Config: cfg, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	controlAddr := application.ControlAddr()
	proxyAddr := application.ProxyAddr()
	if controlAddr == "" || proxyAddr == "" {
		t.Fatalf("addresses after Start: control=%q proxy=%q", controlAddr, proxyAddr)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := application.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := application.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	dialFails(t, controlAddr)
	dialFails(t, proxyAddr)
	waitForNoModuleGoroutines(t)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines after Stop = %d, before Start = %d", after, before)
	}
}

func TestAppSnapshotProvider(t *testing.T) {
	cfg := testConfig(t)
	active := &contract.Active{Link: contract.Link{Name: "tokyo", Server: "203.0.113.1", Port: 443}}
	application, err := New(Options{
		Version: "test",
		Config:  cfg,
		Stderr:  io.Discard,
		Snapshot: func() contract.Snapshot {
			return contract.Snapshot{Active: active, Ready: []contract.Ready{{}, {}}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer application.Stop(context.Background())

	snap := application.Snapshot()
	if snap.Active == nil || snap.Active.Link.Name != "tokyo" {
		t.Fatalf("snapshot active = %+v", snap.Active)
	}
	if snap.Generation != 1 {
		t.Fatalf("generation = %d, want 1", snap.Generation)
	}
	if len(snap.Ready) != 2 {
		t.Fatalf("ready = %d", len(snap.Ready))
	}
}

func TestAppApplyConfigTransactional(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(Options{Version: "test", Config: cfg, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Stop(context.Background())

	next := cfg.Clone()
	next.Cycle.Interval = 2 * time.Hour
	next.Cycle.ReadyTarget = 4
	next.Swap.PromotionTimeout = 10 * time.Second
	next.Log.Level = "debug"
	if err := application.ApplyConfig(next); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	current := application.Config().Current()
	if current.Generation != 2 {
		t.Fatalf("generation = %d, want 2", current.Generation)
	}
	if current.Config.Cycle.Interval != 2*time.Hour {
		t.Fatalf("interval = %v", current.Config.Cycle.Interval)
	}
	if current.Config.Cycle.ReadyTarget != 4 {
		t.Fatalf("ready_target = %d", current.Config.Cycle.ReadyTarget)
	}
	if got := application.logs.Level(); got != slog.LevelDebug {
		t.Fatalf("log level = %v, want debug", got)
	}

	rejected := []struct {
		name   string
		mutate func(*config.Config)
		field  string
	}{
		{"invalid", func(c *config.Config) { c.Server.PanelListen = "broken" }, "panel_listen"},
		{"panel", func(c *config.Config) { c.Server.PanelListen = "127.0.0.1:1" }, "panel_listen"},
		{"proxy", func(c *config.Config) { c.Server.ProxyListen = "127.0.0.1:1" }, "proxy_listen"},
		{"tester", func(c *config.Config) { c.Test.TCPTimeout = time.Second }, "test.*"},
		{"egress", func(c *config.Config) { c.Test.EgressProxy = "http://127.0.0.1:1" }, "egress_proxy"},
		{"urls", func(c *config.Config) { c.URLs.Probe = []string{"https://example.invalid/probe"} }, "urls.probe"},
		{"state", func(c *config.Config) { c.Paths.StateFile = filepath.Join(t.TempDir(), "other.json") }, "state_file"},
		{"xray", func(c *config.Config) { c.Paths.XrayBin = "/bin/false" }, "xray_bin"},
		{"urls-file", func(c *config.Config) { c.Paths.URLsFile = filepath.Join(t.TempDir(), "other.txt") }, "urls_file"},
		{"log-dir", func(c *config.Config) { c.Paths.LogDir = t.TempDir() }, "log_dir"},
	}
	for _, tc := range rejected {
		candidate := next.Clone()
		tc.mutate(&candidate)
		err := application.ApplyConfig(candidate)
		if err == nil {
			t.Fatalf("%s: ApplyConfig accepted a change that requires restart", tc.name)
		}
		if !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("%s: error %q does not name %q", tc.name, err, tc.field)
		}
		if got := application.Config().Current().Generation; got != 2 {
			t.Fatalf("%s: generation changed to %d after rejected apply", tc.name, got)
		}
	}
}

func TestAppReloadConfigFile(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(t.TempDir(), "dissident.conf")
	writeConfig := func(level, panel, extra string) {
		t.Helper()
		body := fmt.Sprintf(`[server]
panel_listen = %s
proxy_listen = %s

[paths]
urls_file = %s
state_file = %s
log_dir = %s
xray_bin = %s

[cycle]
interval = 1h

[log]
level = %s
%s`, panel, cfg.Server.ProxyListen, cfg.Paths.URLsFile, cfg.Paths.StateFile, cfg.Paths.LogDir, cfg.Paths.XrayBin, level, extra)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	writeConfig("warn", cfg.Server.PanelListen, "")
	application, err := New(Options{Version: "test", Config: cfg, ConfigPath: path, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := application.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer application.Stop(context.Background())

	if err := application.ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	current := application.Config().Current()
	if current.Generation != 2 || current.Config.Log.Level != "warn" {
		t.Fatalf("config after reload = gen %d level %q", current.Generation, current.Config.Log.Level)
	}

	// A reload that would need a new immutable tester is rejected, and the
	// previous generation keeps serving.
	writeConfig("debug", cfg.Server.PanelListen, "\n[test]\ntcp_timeout = 1s\n")
	if err := application.ReloadConfig(); err == nil || !strings.Contains(err.Error(), "test.*") {
		t.Fatalf("ReloadConfig tester change error = %v", err)
	}
	if got := application.Config().Current().Generation; got != 2 {
		t.Fatalf("generation changed after rejected tester reload: %d", got)
	}

	if err := os.WriteFile(path, []byte("[server]\npanel_listen = broken\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if err := application.ReloadConfig(); err == nil {
		t.Fatal("ReloadConfig accepted an invalid file")
	}
	if application.Config().Current().Generation != 2 {
		t.Fatalf("generation changed after failed reload: %d", application.Config().Current().Generation)
	}
}
