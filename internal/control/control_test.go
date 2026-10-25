package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

const (
	testSecret = "S3CR3T-password-9f3a"
	testUUID   = "11111111-2222-3333-4444-555555555555"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testLink() contract.Link {
	return contract.Link{
		Raw:      "vless://" + testUUID + ":" + testSecret + "@198.51.100.10:443?security=reality#" + testSecret,
		Hash:     strings.Repeat("ab", 32),
		Protocol: contract.ProtocolVLESS,
		Name:     "tokyo-1",
		Server:   "198.51.100.10",
		Port:     443,
		Params:   map[string]string{"password": testSecret, "security": "reality"},
	}
}

type fakeData struct {
	mu sync.Mutex

	snap    contract.Snapshot
	entries []contract.Entry
	cycling bool
	last    time.Time
	next    time.Time
	lastErr string
	subs    []string

	replaceErr   error
	removeErr    error
	addErr       error
	removeSubErr error
	runErr       error

	replaceCalls    int
	removeCalls     int
	runCalls        int
	cancelCalls     int
	addCalls        int
	removeSubCalls  int
	replaceDeadline bool
	removeHash      string
	addArg          string
	removeSubArg    string

	runStarted chan struct{}
	runRelease chan struct{}
	runDone    chan struct{}
	runOnce    sync.Once
}

func newFakeData(snap contract.Snapshot) *fakeData {
	return &fakeData{
		snap:       snap,
		runStarted: make(chan struct{}),
		runRelease: make(chan struct{}),
		runDone:    make(chan struct{}),
	}
}

func (f *fakeData) Snapshot() contract.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeData) Entries() []contract.Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contract.Entry(nil), f.entries...)
}

func (f *fakeData) Cycling() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cycling
}

func (f *fakeData) LastCycle() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

func (f *fakeData) NextCycle() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

func (f *fakeData) LastError() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastErr
}

func (f *fakeData) Replace(ctx context.Context) error {
	f.mu.Lock()
	f.replaceCalls++
	_, f.replaceDeadline = ctx.Deadline()
	err := f.replaceErr
	f.mu.Unlock()
	return err
}

func (f *fakeData) RunCycle(ctx context.Context) error {
	f.mu.Lock()
	f.runCalls++
	started, release, done, err := f.runStarted, f.runRelease, f.runDone, f.runErr
	f.mu.Unlock()
	if started != nil {
		f.runOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			if done != nil {
				close(done)
			}
			return ctx.Err()
		}
	}
	if done != nil {
		close(done)
	}
	return err
}

func (f *fakeData) CancelCycle() {
	f.mu.Lock()
	f.cancelCalls++
	f.mu.Unlock()
}

func (f *fakeData) RemoveReady(hash string) error {
	f.mu.Lock()
	f.removeCalls++
	f.removeHash = hash
	err := f.removeErr
	f.mu.Unlock()
	return err
}

func (f *fakeData) Subscriptions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subs...)
}

func (f *fakeData) AddSubscription(raw string) error {
	f.mu.Lock()
	f.addCalls++
	f.addArg = raw
	err := f.addErr
	if err == nil {
		f.subs = append(f.subs, raw)
	}
	f.mu.Unlock()
	return err
}

func (f *fakeData) RemoveSubscription(raw string) error {
	f.mu.Lock()
	f.removeSubCalls++
	f.removeSubArg = raw
	err := f.removeSubErr
	if err == nil {
		out := f.subs[:0]
		for _, s := range f.subs {
			if s != raw {
				out = append(out, s)
			}
		}
		f.subs = out
	}
	f.mu.Unlock()
	return err
}

func (f *fakeData) setCycling(v bool) {
	f.mu.Lock()
	f.cycling = v
	f.mu.Unlock()
}

func (f *fakeData) counts() (replace, remove, run, cancel, add, removeSub int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceCalls, f.removeCalls, f.runCalls, f.cancelCalls, f.addCalls, f.removeSubCalls
}

func (f *fakeData) replaceHadDeadline() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replaceDeadline
}

func testConfig(t *testing.T) *config.Manager {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.PanelListen = "127.0.0.1:0"
	cfg.URLs.Probe = []string{"https://" + testSecret + ":" + testSecret + "@probe.example/generate_204"}
	cfg.URLs.Verify = []string{"https://verify.example/generate_204"}
	cfg.URLs.Speed = "https://speed.example/file"
	cfg.URLs.ExitIP = "https://" + testSecret + "@exit.example/"
	mgr, err := config.NewManager(cfg)
	if err != nil {
		t.Fatalf("config.NewManager: %v", err)
	}
	return mgr
}

func testSnapshot(t *testing.T) contract.Snapshot {
	t.Helper()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	link := testLink()
	return contract.Snapshot{
		Generation: 12,
		Active: &contract.Active{
			Link:    link,
			Since:   now.Add(-2 * time.Hour),
			ExitIP:  "203.0.113.9",
			XrayPID: 1234,
		},
		Ready: []contract.Ready{
			{
				Link:      link,
				Score:     91.5,
				LastCheck: now,
				ExitIP:    "203.0.113.9",
			},
			{
				Link: contract.Link{
					Raw:      "trojan://" + testSecret + "@198.51.100.11:8443#osaka",
					Hash:     strings.Repeat("cd", 32),
					Protocol: contract.ProtocolTrojan,
					Name:     "osaka-2",
					Server:   "198.51.100.11",
					Port:     8443,
				},
				Score:     77,
				LastCheck: now.Add(-5 * time.Minute),
				ExitIP:    "203.0.113.10",
			},
		},
		CatalogCount:   30,
		SupportedCount: 20,
		Testing:        1,
		Updated:        now,
	}
}

func testEntries(t *testing.T) []contract.Entry {
	t.Helper()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	link := testLink()
	link.Name = "tokyo-1 <script>alert(1)</script>"
	return []contract.Entry{
		{
			Link:  link,
			State: contract.StateTesting,
			Score: 50,
			Last: contract.Result{
				Hash:       link.Hash,
				Stage:      contract.StageTLS,
				OK:         false,
				Reason:     "tcp failed for " + link.Raw + " uuid " + testUUID,
				At:         now,
				TCPLatency: 42 * time.Millisecond,
				SpeedMbps:  12.5,
			},
			Updated: now,
		},
		{
			Link:  testLink(),
			State: contract.StateReady,
			Score: 91.5,
			Last: contract.Result{
				Hash:  testLink().Hash,
				Stage: contract.StageStability,
				OK:    true,
				At:    now.Add(-time.Minute),
			},
			Updated: now.Add(-time.Minute),
		},
		{
			Link: contract.Link{
				Raw:      "ss://" + testSecret + "@198.51.100.12:8388#failed",
				Hash:     strings.Repeat("ef", 32),
				Protocol: contract.ProtocolShadowsocks,
				Name:     "failed-3",
				Server:   "198.51.100.12",
				Port:     8388,
			},
			State: contract.StateFailed,
			Last: contract.Result{
				Stage:  contract.StageTCP,
				OK:     false,
				Reason: "timeout",
				At:     now.Add(-time.Hour),
			},
			Updated: now.Add(-time.Hour),
		},
		{
			Link: contract.Link{
				Name:   "catalog-only",
				Server: "198.51.100.13",
				Port:   443,
				Hash:   strings.Repeat("12", 32),
			},
			State:   contract.StateCatalog,
			Updated: now.Add(-2 * time.Hour),
		},
	}
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	if opts.Logger == nil {
		opts.Logger = testLogger()
	}
	return New(opts)
}

func serve(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func postForm(t *testing.T, handler http.Handler, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertNoSecrets(t *testing.T, body string) {
	t.Helper()
	for _, needle := range []string{
		testSecret,
		testUUID,
		"vless://",
		"vmess://",
		"trojan://",
		"ss://",
		"socks5://",
		strings.Repeat("ab", 32),
	} {
		if strings.Contains(body, needle) {
			t.Errorf("page leaks %q:\n%s", needle, body)
		}
	}
}

func TestHealthAndReadinessWithoutActiveWAN(t *testing.T) {
	snap := contract.Snapshot{Generation: 3}
	handler := newTestServer(t, Options{
		Snapshot: func() contract.Snapshot { return snap },
	}).Handler()

	health := serve(t, handler, http.MethodGet, "/healthz")
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", health.Code, health.Body.String())
	}

	ready := serve(t, handler, http.MethodGet, "/readyz")
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", ready.Code)
	}
	if !strings.Contains(ready.Body.String(), "no active WAN") {
		t.Fatalf("readyz body = %q", ready.Body.String())
	}

	snap.Active = &contract.Active{Link: testLink()}
	ready = serve(t, handler, http.MethodGet, "/readyz")
	if ready.Code != http.StatusOK || ready.Body.String() != "ready\n" {
		t.Fatalf("readyz with active WAN = %d %q", ready.Code, ready.Body.String())
	}
}

func TestPagesRenderAndRedact(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access.log")
	systemPath := filepath.Join(dir, "dissident.log")
	access := strings.Join([]string{
		"access-line-marker GET /",
		"2026-01-02T03:04:05.000Z INFO " + testLink().Raw,
		"2026-01-02T03:04:06.000Z http https://" + testSecret + ":" + testSecret + "@example.com/path",
		"2026-01-02T03:04:07.000Z raw " + testSecret + ":" + testSecret + "@host:443",
	}, "\n") + "\n"
	system := "system-line-marker level=info " + testLink().Raw + "\n"
	if err := os.WriteFile(accessPath, []byte(access), 0o600); err != nil {
		t.Fatalf("write access log: %v", err)
	}
	if err := os.WriteFile(systemPath, []byte(system), 0o600); err != nil {
		t.Fatalf("write system log: %v", err)
	}

	link := testLink()
	fake := newFakeData(testSnapshot(t))
	fake.entries = testEntries(t)
	fake.lastErr = "cycle failed: " + link.Raw
	fake.last = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	fake.next = time.Date(2026, 1, 2, 3, 30, 0, 0, time.UTC)
	fake.subs = []string{
		"https://" + testSecret + ":" + testSecret + "@subs.example/list.txt",
		link.Raw,
		"https://subs.example/plain.txt",
		"ftp://local.example/file",
	}

	server := newTestServer(t, Options{
		Data:      fake,
		Config:    testConfig(t),
		AccessLog: accessPath,
		SystemLog: systemPath,
	})
	handler := server.Handler()

	cases := []struct {
		path  string
		wants []string
	}{
		{"/", []string{"Overview", "Active WAN", "tokyo-1", "Config generation", "http-equiv=\"refresh\"", "Top ready entries", "Recent test results"}},
		{"/wan", []string{"Replace active", "Ready standbys", "Remove", "tokyo-1", "osaka-2"}},
		{"/tests", []string{"Run cycle", "Cancel", "Qualification results", "tls", "Catalog entries"}},
		{"/subscriptions", []string{"Subscriptions", "Add subscription", "2 local links", "https://subs.example/plain.txt"}},
		{"/settings", []string{"Settings", "server.panel_listen", "SIGHUP", "urls.probe"}},
		{"/logs?type=access", []string{"Access log", "System log", "access-line-marker", "Filter substring"}},
		{"/logs?type=system", []string{"System log", "system-line-marker"}},
	}
	for _, tc := range cases {
		rec := serve(t, handler, http.MethodGet, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200: %s", tc.path, rec.Code, rec.Body.String())
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s content type = %q", tc.path, ct)
		}
		body := rec.Body.String()
		for _, want := range tc.wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", tc.path, want)
			}
		}
		assertNoSecrets(t, body)
	}

	overview := serve(t, handler, http.MethodGet, "/").Body.String()
	if !strings.Contains(overview, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("overview did not HTML-escape the entry name")
	}
	if strings.Contains(overview, "<script>alert(1)</script>") {
		t.Error("overview rendered unescaped entry name markup")
	}

	flash := serve(t, handler, http.MethodGet, "/wan?msg="+url.QueryEscape("bad "+testLink().Raw)+"&err=1").Body.String()
	if !strings.Contains(flash, "flash-error") {
		t.Error("error flash did not use error styling")
	}
	assertNoSecrets(t, flash)
}

func TestReplaceDisabledWhenReadyEmpty(t *testing.T) {
	fake := newFakeData(contract.Snapshot{Generation: 1})
	fake.entries = nil
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()
	body := serve(t, handler, http.MethodGet, "/wan").Body.String()
	if !strings.Contains(body, "disabled>Replace active") {
		t.Fatalf("Replace active is not disabled with no ready entries:\n%s", body)
	}
}

func TestWANActions(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	fake.entries = testEntries(t)
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()

	rec := postForm(t, handler, "/wan/replace", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("replace status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/wan" || loc.Query().Get("msg") != "active WAN replaced" {
		t.Fatalf("replace redirect = %q (%v)", rec.Header().Get("Location"), err)
	}
	replace, remove, _, _, _, _ := fake.counts()
	if replace != 1 || remove != 0 {
		t.Fatalf("calls after replace = %d,%d", replace, remove)
	}
	if !fake.replaceHadDeadline() {
		t.Fatal("Replace was called without a deadline")
	}

	fullHash := strings.Repeat("ab", 32)
	rec = postForm(t, handler, "/wan/remove", url.Values{"hash": []string{fullHash[:12]}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove status = %d", rec.Code)
	}
	fake.mu.Lock()
	gotHash := fake.removeHash
	fake.mu.Unlock()
	if gotHash != fullHash {
		t.Fatalf("RemoveReady hash = %q, want %q", gotHash, fullHash)
	}

	rec = postForm(t, handler, "/wan/remove", url.Values{"hash": []string{"ffffffffffff"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unknown remove status = %d", rec.Code)
	}
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("unknown remove was not an error flash: %q", loc.String())
	}

	fake.mu.Lock()
	fake.replaceErr = errors.New("replace exploded for " + testLink().Raw)
	fake.mu.Unlock()
	rec = postForm(t, handler, "/wan/replace", nil, nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("replace error was not an error flash: %q", loc.String())
	}
	if strings.Contains(loc.RawQuery, testSecret) || strings.Contains(loc.RawQuery, "vless://") {
		t.Fatalf("flash leaked credentials: %q", loc.RawQuery)
	}
}

func TestCycleActions(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()

	rec := postForm(t, handler, "/cycle/run", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("run status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Path != "/tests" || loc.Query().Get("msg") != "cycle run requested" {
		t.Fatalf("run redirect = %q", rec.Header().Get("Location"))
	}
	select {
	case <-fake.runStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCycle was not called")
	}
	close(fake.runRelease)
	select {
	case <-fake.runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RunCycle did not finish after release")
	}

	fake.setCycling(true)
	rec = postForm(t, handler, "/cycle/run", nil, nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("msg") != "a cycle is already running" {
		t.Fatalf("already-running redirect = %q", loc.String())
	}
	fake.setCycling(false)

	rec = postForm(t, handler, "/cycle/cancel", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("cancel status = %d", rec.Code)
	}
	if _, _, _, cancel, _, _ := fake.counts(); cancel != 1 {
		t.Fatalf("CancelCycle calls = %d", cancel)
	}
}

func TestSubscriptionActions(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	fake.subs = []string{"https://subs.example/one.txt"}
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()

	rec := postForm(t, handler, "/subscriptions/add", url.Values{"url": []string{"https://example.com/sub"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add status = %d", rec.Code)
	}
	if _, _, _, _, add, _ := fake.counts(); add != 1 {
		t.Fatalf("AddSubscription calls = %d", add)
	}
	fake.mu.Lock()
	added := fake.addArg
	fake.mu.Unlock()
	if added != "https://example.com/sub" {
		t.Fatalf("AddSubscription argument = %q", added)
	}

	rec = postForm(t, handler, "/subscriptions/add", url.Values{"url": []string{"ftp://example.com/sub"}}, nil)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("invalid add was not rejected: %q", loc.String())
	}
	if _, _, _, _, add, _ := fake.counts(); add != 1 {
		t.Fatalf("invalid add reached AddSubscription (%d calls)", add)
	}

	fake.mu.Lock()
	first := fake.subs[0]
	fake.mu.Unlock()
	key := subKey(first)
	rec = postForm(t, handler, "/subscriptions/remove", url.Values{"key": []string{key}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("remove status = %d", rec.Code)
	}
	fake.mu.Lock()
	removed := fake.removeSubArg
	fake.mu.Unlock()
	if removed != "https://subs.example/one.txt" {
		t.Fatalf("RemoveSubscription argument = %q", removed)
	}

	rec = postForm(t, handler, "/subscriptions/remove", url.Values{"key": []string{"deadbeefdeadbeef"}}, nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("unknown remove was not an error: %q", loc.String())
	}

	fake.mu.Lock()
	fake.addErr = errors.New("add failed " + testLink().Raw)
	fake.mu.Unlock()
	rec = postForm(t, handler, "/subscriptions/add", url.Values{"url": []string{"https://example.com/secret"}}, nil)
	loc, _ = url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("add error was not an error flash: %q", loc.String())
	}
	if strings.Contains(loc.RawQuery, testSecret) || strings.Contains(loc.RawQuery, "vless://") {
		t.Fatalf("add flash leaked credentials: %q", loc.RawQuery)
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()
	rec := postForm(t, handler, "/wan/replace", nil, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site status = %d, want 403", rec.Code)
	}
	_, _, _, _, _, _ = fake.counts()
	if fake.replaceCalls != 0 {
		t.Fatal("cross-site POST reached Replace")
	}
}

func TestMetrics(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	fake.setCycling(true)
	fake.last = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()

	rec := serve(t, handler, http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("metrics content type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"dissident_up 1",
		"dissident_config_generation 12",
		"dissident_active_wan 1",
		"dissident_ready_entries 2",
		"dissident_catalog_entries 30",
		"dissident_supported_entries 20",
		"dissident_testing_entries 1",
		"dissident_cycle_running 1",
		"dissident_last_cycle_timestamp 1767322800",
		"# TYPE dissident_up gauge",
		"dissident_build_info{version=\"test\",goversion=\"go",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics does not contain %q:\n%s", want, body)
		}
	}
	assertNoSecrets(t, body)
}

func TestLogsPage(t *testing.T) {
	dir := t.TempDir()
	accessPath := filepath.Join(dir, "access.log")
	missingPath := filepath.Join(dir, "missing.log")

	var lines []string
	for i := 1; i <= 1200; i++ {
		lines = append(lines, fmt.Sprintf("line-%04d", i))
	}
	lines = append(lines, "needle-alpha", "line-needle-beta")
	if err := os.WriteFile(accessPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	handler := newTestServer(t, Options{
		AccessLog: accessPath,
		SystemLog: missingPath,
	}).Handler()

	rec := serve(t, handler, http.MethodGet, "/logs?type=access&lines=100000")
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "line-0203") || !strings.Contains(body, "line-1200") {
		t.Error("tail did not include the expected last 1000 lines")
	}
	if strings.Contains(body, "line-0202") {
		t.Error("tail returned more than 1000 lines")
	}
	if !strings.Contains(body, "at most the final 2 MiB") {
		t.Error("logs page did not explain the bounded read")
	}

	body = serve(t, handler, http.MethodGet, "/logs?type=access&lines=10&filter=needle").Body.String()
	if !strings.Contains(body, "needle-alpha") || !strings.Contains(body, "line-needle-beta") {
		t.Error("filtered log view is missing matching lines")
	}
	if strings.Contains(body, "line-1200") {
		t.Error("filtered log view included a non-matching line")
	}
	if !strings.Contains(body, "type=system") || !strings.Contains(body, "filter=needle") {
		t.Error("tabs did not preserve lines/filter parameters")
	}

	body = serve(t, handler, http.MethodGet, "/logs?type=system").Body.String()
	if !strings.Contains(body, "does not exist yet") {
		t.Errorf("missing system log message not shown:\n%s", body)
	}

	unset := newTestServer(t, Options{}).Handler()
	body = serve(t, unset, http.MethodGet, "/logs").Body.String()
	if !strings.Contains(body, "No access log file is configured") {
		t.Errorf("unset access log message not shown:\n%s", body)
	}
}

func TestReadOnlyModeWithoutData(t *testing.T) {
	snap := testSnapshot(t)
	handler := newTestServer(t, Options{
		Snapshot: func() contract.Snapshot { return snap },
	}).Handler()

	for _, path := range []string{"/", "/wan", "/tests", "/subscriptions", "/settings"} {
		rec := serve(t, handler, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
	wan := serve(t, handler, http.MethodGet, "/wan").Body.String()
	if !strings.Contains(wan, "disabled>Replace active") {
		t.Error("read-only WAN page did not disable Replace active")
	}
	if !strings.Contains(wan, "read-only Snapshot mode") {
		t.Error("read-only WAN page did not explain the missing provider")
	}
	settings := serve(t, handler, http.MethodGet, "/settings").Body.String()
	if !strings.Contains(settings, "No configuration manager is attached") {
		t.Error("read-only settings page did not explain the missing config")
	}

	rec := postForm(t, handler, "/wan/replace", nil, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("read-only replace status = %d, want 303", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("err") != "1" {
		t.Fatalf("read-only replace was not an error flash: %q", loc.String())
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	fake := newFakeData(testSnapshot(t))
	handler := newTestServer(t, Options{Data: fake, Config: testConfig(t)}).Handler()

	if rec := serve(t, handler, http.MethodGet, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", rec.Code)
	}
	if rec := serve(t, handler, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / status = %d", rec.Code)
	}
	if rec := serve(t, handler, http.MethodGet, "/wan/replace"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /wan/replace status = %d", rec.Code)
	}
	if rec := serve(t, handler, http.MethodPost, "/wan"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /wan status = %d", rec.Code)
	}
}

func TestHandlerPanicIsRecovered(t *testing.T) {
	server := newTestServer(t, Options{
		Snapshot: func() contract.Snapshot {
			panic("snapshot provider exploded")
		},
	})
	for _, path := range []string{"/readyz", "/"} {
		rec := serve(t, server.Handler(), http.MethodGet, path)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s panicking handler status = %d, want 500", path, rec.Code)
		}
	}
}

func TestStartServeStop(t *testing.T) {
	server := newTestServer(t, Options{})
	ctx := context.Background()
	if err := server.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := server.Addr()
	if addr == "" || strings.HasSuffix(addr, ":0") {
		t.Fatalf("Addr = %q", addr)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}

	resp, err = client.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", resp.StatusCode)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := server.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop should fail")
	}
}

func TestReloadRejectsListenChange(t *testing.T) {
	server := newTestServer(t, Options{Listen: "127.0.0.1:8090"})
	cfg := config.Defaults()
	cfg.Server.PanelListen = "127.0.0.1:8090"
	snap := &config.Snapshot{Generation: 2, Config: cfg}
	if err := server.Reload(snap); err != nil {
		t.Fatalf("Reload same listen: %v", err)
	}
	changed := config.Defaults()
	changed.Server.PanelListen = "127.0.0.1:9090"
	err := server.Reload(&config.Snapshot{Generation: 3, Config: changed})
	if err == nil || !strings.Contains(err.Error(), "requires restart") {
		t.Fatalf("Reload changed listen error = %v", err)
	}
	if err := server.Reload(nil); err == nil {
		t.Fatal("Reload(nil) should fail")
	}

	other := config.Defaults()
	other.Server.PanelListen = "127.0.0.1:8090"
	other.Cycle.Interval = 5 * time.Minute
	if err := server.Reload(&config.Snapshot{Generation: 4, Config: other}); err != nil {
		t.Fatalf("Reload other changes: %v", err)
	}
}
