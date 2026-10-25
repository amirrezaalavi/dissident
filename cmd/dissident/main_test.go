package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dissident "+version) {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestRunCheckConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dissident.conf")
	if err := os.WriteFile(path, []byte("[log]\nlevel = info\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-check-config", "-config", path}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "config OK") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunConfigError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.conf")
	if err := os.WriteFile(path, []byte("[server]\npanel_listen = broken\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-check-config", "-config", path}, &stdout, &stderr); code != exitConfig {
		t.Fatalf("exit = %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr.String(), "config error") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-not-a-flag"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if code := run([]string{"extra"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("positional exit = %d, want %d", code, exitUsage)
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr.String(), "usage: dissident") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
