package xraycfg

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dissident/internal/uri"
)

const realXrayBin = "/usr/local/bin/xray"

// fakeXray writes a small executable named fake-xray into a temp dir. The
// script body receives the real xray arguments: $1=-test $2=-c $3=config.
func fakeXray(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake xray uses a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "fake-xray")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}
	return path
}

func TestPreflightWritesReadOnlyTempFileAndCleansUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode assertion uses stat -c, available on linux")
	}
	dir := t.TempDir()
	recordMode := filepath.Join(dir, "mode")
	recordCfg := filepath.Join(dir, "cfg")
	bin := fakeXray(t,
		"stat -c %a \"$3\" > '"+recordMode+"'\n"+
			"cat \"$3\" > '"+recordCfg+"'\n"+
			"exit 0\n")

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	cfg := []byte("{\"log\":{\"loglevel\":\"warning\"}}")
	if err := Preflight(context.Background(), bin, cfg); err != nil {
		t.Fatalf("Preflight error: %v", err)
	}

	mode, err := os.ReadFile(recordMode)
	if err != nil {
		t.Fatalf("read recorded mode: %v", err)
	}
	if got := strings.TrimSpace(string(mode)); got != "600" {
		t.Fatalf("temp config mode = %q, want 600", got)
	}
	copied, err := os.ReadFile(recordCfg)
	if err != nil {
		t.Fatalf("read copied config: %v", err)
	}
	if string(copied) != string(cfg) {
		t.Fatalf("xray saw config %q, want %q", copied, cfg)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp config was not removed: %v", entries)
	}
}

func TestPreflightReturnsCombinedOutputOnFailure(t *testing.T) {
	bin := fakeXray(t, "echo \"fake xray failure\" >&2\nexit 7\n")
	err := Preflight(context.Background(), bin, []byte("{}"))
	if err == nil {
		t.Fatal("Preflight succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "fake xray failure") {
		t.Fatalf("error = %q, want combined xray output", err)
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Fatalf("error = %q, want a failure description", err)
	}
}

func TestPreflightTimeout(t *testing.T) {
	bin := fakeXray(t, "exec sleep 10\n")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Preflight(ctx, bin, []byte("{}"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Preflight succeeded, want timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %q, want a timeout description", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Preflight took %v, timeout was not enforced", elapsed)
	}
}

func TestPreflightArgumentValidation(t *testing.T) {
	if err := Preflight(context.Background(), "", []byte("{}")); err == nil {
		t.Fatal("empty binary path succeeded")
	}
	if err := Preflight(context.Background(), realXrayBin, nil); err == nil {
		t.Fatal("empty config succeeded")
	}
	if err := Preflight(context.Background(), filepath.Join(t.TempDir(), "missing-xray"), []byte("{}")); err == nil {
		t.Fatal("missing binary succeeded")
	}
}

// TestPreflightRealXrayAllSupported is the real schema check: every supported
// protocol/transport combination rendered by Render must pass xray -test. It
// only runs when /usr/local/bin/xray is installed.
func TestPreflightRealXrayAllSupported(t *testing.T) {
	if _, err := os.Stat(realXrayBin); err != nil {
		t.Skipf("xray binary not installed at %s: %v", realXrayBin, err)
	}
	for _, tc := range supportedRawCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			link, err := uri.Parse(tc.raw)
			if err != nil {
				t.Fatalf("uri.Parse: %v", err)
			}
			cfg, err := Render(link, 10800)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := Preflight(ctx, realXrayBin, cfg); err != nil {
				t.Fatalf("xray -test rejected rendered config: %v\n%s", err, cfg)
			}
		})
	}
}

func TestPreflightRealXrayRejectsInvalidConfig(t *testing.T) {
	if _, err := os.Stat(realXrayBin); err != nil {
		t.Skipf("xray binary not installed at %s: %v", realXrayBin, err)
	}
	err := Preflight(context.Background(), realXrayBin, []byte("this is not xray json"))
	if err == nil {
		t.Fatal("Preflight accepted invalid JSON, want failure")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("error = %q, want a preflight error", err)
	}
}
