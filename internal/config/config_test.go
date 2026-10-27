package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dissident.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}

	wantXray := "/usr/local/bin/xray"
	if runtime.GOOS == "windows" {
		wantXray = "xray.exe"
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"panel listen", cfg.Server.PanelListen, "0.0.0.0:8090"},
		{"proxy listen", cfg.Server.ProxyListen, "0.0.0.0:10909"},
		{"urls file", cfg.Paths.URLsFile, "./configs/urls.txt"},
		{"state file", cfg.Paths.StateFile, "./var/state.json"},
		{"log dir", cfg.Paths.LogDir, "./var/log"},
		{"xray bin", cfg.Paths.XrayBin, wantXray},
		{"cycle interval", cfg.Cycle.Interval, 1800 * time.Second},
		{"fetch timeout", cfg.Cycle.FetchTimeout, 20 * time.Second},
		{"active liveness", cfg.Cycle.ActiveLiveness, 15 * time.Second},
		{"probe count", cfg.Cycle.ProbeCount, 3},
		{"probe window", cfg.Cycle.ProbeWindow, 8 * time.Second},
		{"ready min", cfg.Cycle.ReadyMin, 3},
		{"ready target", cfg.Cycle.ReadyTarget, 5},
		{"failed retry", cfg.Cycle.FailedRetry, 30 * time.Minute},
		{"drain timeout", cfg.Swap.DrainTimeout, 30 * time.Second},
		{"cheap workers", cfg.Test.CheapWorkers, 8},
		{"speed size", cfg.Test.SpeedSize, int64(1000000)},
		{"min speed", cfg.Test.MinSpeedMbps, 1.0},
		{"content quorum", cfg.Test.ContentQuorum, 3},
		{"content urls", len(cfg.URLs.Content), 4},
		{"log level", cfg.Log.Level, "info"},
		{"log access", cfg.Log.Access, true},
		{"retention days", cfg.Log.RetentionDays, 30},
		{"max size mb", cfg.Log.MaxSizeMB, 100},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadSectionsAndDottedKeys(t *testing.T) {
	body := `
# top-level dotted keys are accepted
server.proxy_listen = 127.0.0.1:19009
log.level = debug

[server]
panel_listen = 127.0.0.1:18090

[paths]
log_dir = "/tmp/dissident-logs"
xray_bin = /opt/xray

[cycle]
interval = 60s
fetch_timeout = 25s
ready_target = 4
failed_retry = 45m

[test]
content_quorum = 2
egress_proxy = http://127.0.0.1:10808

[urls]
content = https://example.com, https://example.org

[log]
access = false
`
	cfg, err := Load(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.PanelListen != "127.0.0.1:18090" {
		t.Errorf("panel listen = %q", cfg.Server.PanelListen)
	}
	if cfg.Server.ProxyListen != "127.0.0.1:19009" {
		t.Errorf("proxy listen = %q", cfg.Server.ProxyListen)
	}
	if cfg.Cycle.Interval != time.Minute {
		t.Errorf("cycle interval = %v", cfg.Cycle.Interval)
	}
	if cfg.Cycle.ReadyTarget != 4 || cfg.Cycle.ReadyMin != 3 {
		t.Errorf("ready target/min = %d/%d", cfg.Cycle.ReadyTarget, cfg.Cycle.ReadyMin)
	}
	if cfg.Cycle.FailedRetry != 45*time.Minute {
		t.Errorf("failed retry = %v", cfg.Cycle.FailedRetry)
	}
	if cfg.Cycle.FetchTimeout != 25*time.Second {
		t.Errorf("fetch timeout = %v", cfg.Cycle.FetchTimeout)
	}
	if got := len(cfg.URLs.Content); got != 2 {
		t.Errorf("content urls = %d, want 2", got)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Access {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.Paths.LogDir != "/tmp/dissident-logs" {
		t.Errorf("log dir = %q", cfg.Paths.LogDir)
	}
	if cfg.Test.EgressProxy != "http://127.0.0.1:10808" {
		t.Errorf("egress proxy = %q", cfg.Test.EgressProxy)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	body := "[server]\npanel_listen = 127.0.0.1:8090\nnope = true\n"
	_, err := Load(writeTempConfig(t, body))
	if err == nil {
		t.Fatal("expected an error for an unknown key")
	}
	if !strings.Contains(err.Error(), "unknown config key \"server.nope\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "bad listener",
			body: "[server]\npanel_listen = not-a-listener\n",
			want: "server.panel_listen",
		},
		{
			name: "listeners collide",
			body: "[server]\nproxy_listen = 0.0.0.0:8090\n",
			want: "must differ",
		},
		{
			name: "non-http probe url",
			body: "[urls]\nprobe = ftp://example.com/file\n",
			want: "scheme must be http or https",
		},
		{
			name: "malformed duration",
			body: "[cycle]\ninterval = soon\n",
			want: "expected a duration",
		},
		{
			name: "quorum exceeds content",
			body: "[test]\ncontent_quorum = 9\n",
			want: "must not exceed",
		},
		{
			name: "empty xray binary",
			body: "[paths]\nxray_bin =\n",
			want: "paths.xray_bin must not be empty",
		},
		{
			name: "bad log level",
			body: "[log]\nlevel = verbose\n",
			want: "log.level must be one of",
		},
		{
			name: "bad bool",
			body: "[log]\naccess = maybe\n",
			want: "expected true or false",
		},
		{
			name: "negative worker count",
			body: "[test]\ncheap_workers = -1\n",
			want: "test.cheap_workers must be at least 1",
		},
		{
			name: "ready min exceeds target",
			body: "[cycle]\nready_min = 6\n",
			want: "must not exceed cycle.ready_target",
		},
		{
			name: "zero retention",
			body: "[log]\nretention_days = 0\n",
			want: "log.retention_days must be at least 1",
		},
		{
			name: "duplicate key",
			body: "[server]\npanel_listen = 127.0.0.1:8090\npanel_listen = 127.0.0.1:8091\n",
			want: "duplicate key",
		},
		{
			name: "malformed line",
			body: "[server]\nthis is not a key value pair\n",
			want: "expected key = value",
		},
		{
			name: "bad egress proxy",
			body: "[test]\negress_proxy = socks5://127.0.0.1:1080\n",
			want: "test.egress_proxy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, tc.body))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.conf"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestManagerFailedApplyPreservesSnapshot(t *testing.T) {
	manager, err := NewManager(Defaults())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	old := manager.Current()
	if old == nil || old.Generation != 1 {
		t.Fatalf("initial snapshot = %+v", old)
	}

	bad := Defaults()
	bad.Server.PanelListen = "broken"
	if err := manager.Apply(bad); err == nil {
		t.Fatal("Apply accepted an invalid config")
	}
	after := manager.Current()
	if after != old {
		t.Fatal("failed Apply replaced the snapshot pointer")
	}
	if after.Generation != 1 {
		t.Fatalf("generation changed to %d after failed apply", after.Generation)
	}
	if after.Config.Server.PanelListen != "0.0.0.0:8090" {
		t.Fatalf("config changed after failed apply: %+v", after.Config.Server)
	}

	good := Defaults()
	good.Cycle.Interval = time.Hour
	if err := manager.Apply(good); err != nil {
		t.Fatalf("Apply valid config: %v", err)
	}
	next := manager.Current()
	if next == after {
		t.Fatal("valid Apply did not replace the snapshot")
	}
	if next.Generation != 2 {
		t.Fatalf("generation = %d, want 2", next.Generation)
	}
	if next.Config.Cycle.Interval != time.Hour {
		t.Fatalf("interval = %v", next.Config.Cycle.Interval)
	}
}

func TestManagerDoesNotAliasCallerSlices(t *testing.T) {
	cfg := Defaults()
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	cfg.URLs.Content[0] = "https://mutated.example"
	if manager.Current().Config.URLs.Content[0] == "https://mutated.example" {
		t.Fatal("manager snapshot aliases the caller's URL slice")
	}
}
