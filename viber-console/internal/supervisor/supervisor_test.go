package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testSleepPath builds a tiny helper script that sleeps, so we can verify
// process lifecycle without a real daemon binary.
func testSleepPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sleeper")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestServiceStartRunningStop(t *testing.T) {
	s := NewService("sleeper", testSleepPath(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !s.IsRunning() {
		t.Fatal("IsRunning = false after start")
	}

	st := s.Status()
	if !st.Running || st.Pid == 0 || st.Restarts != 1 {
		t.Errorf("status = %+v, want running pid>0 restarts=1", st)
	}

	s.Stop()
	if s.IsRunning() {
		t.Fatal("IsRunning = true after stop")
	}
}

func TestServiceRestartIncrements(t *testing.T) {
	s := NewService("sleeper", testSleepPath(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !s.IsRunning() {
		t.Fatal("not running after restart")
	}
	if st := s.Status(); st.Restarts != 2 {
		t.Errorf("restarts = %d, want 2", st.Restarts)
	}
	s.Stop()
}

func TestServiceAutoRestart(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	s := NewService("crasher", "/bin/sh", "", "-c", `printf '%s\n' "$$" >> "$1"; exit 1`, "sh", pidFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := s.Status()
		data, err := os.ReadFile(pidFile)
		pids := strings.Fields(string(data))
		if st.Restarts >= 2 && err == nil && len(pids) >= 2 {
			if pids[0] == pids[1] {
				t.Errorf("restarted child PID = %s, want a different PID from the initial child", pids[1])
			}
			if !st.AutoRestart {
				t.Error("status AutoRestart = false, want true")
			}
			if st.LastRestartAt.IsZero() {
				t.Error("status LastRestartAt is zero after restart")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}

	st := s.Status()
	data, _ := os.ReadFile(pidFile)
	t.Fatalf("status = %+v, recorded PIDs = %q after 3s; want at least one restart", st, data)
}

func TestServiceAutoRestartDisabled(t *testing.T) {
	s := NewService("crasher", "/bin/sh", "", "-c", "exit 1")
	s.AutoRestart = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)

	st := s.Status()
	if st.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", st.Restarts)
	}
	if st.AutoRestart {
		t.Error("status AutoRestart = true, want false")
	}
}

func TestServiceStderrTail(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "printer")
	script := "#!/bin/sh\necho line1 >&2\necho line2 >&2\necho line3 >&2\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := NewService("printer", bin, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	st := s.Status()
	joined := strings.Join(st.Stderr, "|")
	if !strings.Contains(joined, "line1") || !strings.Contains(joined, "line3") {
		t.Errorf("stderr tail = %q, want line1..line3", joined)
	}
	s.Stop()
}

func TestParseEnvFile(t *testing.T) {
	content := "# comment\nKEY1=value1\n\nKEY2 = spaced \nNOVALUE\n"
	env := parseEnvFile(content)
	joined := strings.Join(env, ",")
	if !strings.Contains(joined, "KEY1=value1") {
		t.Errorf("env = %v, want KEY1", env)
	}
	if !strings.Contains(joined, "KEY2 = spaced") {
		t.Errorf("env = %v, want KEY2", env)
	}
	for _, e := range env {
		if strings.HasPrefix(e, "#") || e == "NOVALUE" {
			t.Errorf("bad entry: %q", e)
		}
	}
}

func TestRingBuffer(t *testing.T) {
	r := newRingBuffer(3)
	r.Write("a\nb\nc\nd\n")
	lines := r.Lines()
	if len(lines) != 3 || lines[0] != "b" || lines[2] != "d" {
		t.Errorf("lines = %v, want [b c d]", lines)
	}
}
