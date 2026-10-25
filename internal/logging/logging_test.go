package logging

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dissident/internal/contract"
)

func fixedTime() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func TestFormatAccessRecordBase(t *testing.T) {
	rec := contract.AccessRecord{
		Time:   fixedTime(),
		Src:    "10.0.0.1:5555",
		State:  "active",
		Dst:    "1.2.3.4:443",
		Domain: "example.com",
		Port:   443,
	}
	want := "2026-01-02T03:04:05.000Z\t10.0.0.1:5555\tactive\t1.2.3.4:443\texample.com:443\n"
	if got := FormatAccessRecord(rec); got != want {
		t.Fatalf("access line = %q, want %q", got, want)
	}
}

func TestFormatAccessRecordDetails(t *testing.T) {
	rec := contract.AccessRecord{
		Time:     fixedTime(),
		Src:      "10.0.0.1:5555",
		State:    "active",
		Dst:      "1.2.3.4:443",
		Domain:   "example.com",
		Port:     443,
		Proto:    "vless",
		WAN:      "wan1",
		Up:       1024,
		Down:     2048,
		Duration: 1500 * time.Millisecond,
		Reason:   "ok",
	}
	want := "2026-01-02T03:04:05.000Z\t10.0.0.1:5555\tactive\t1.2.3.4:443\texample.com:443\tvless\twan1\t1024\t2048\t1500\tok\n"
	if got := FormatAccessRecord(rec); got != want {
		t.Fatalf("access line = %q, want %q", got, want)
	}
}

func TestFormatAccessRecordSanitizes(t *testing.T) {
	rec := contract.AccessRecord{
		Time:   fixedTime(),
		Src:    "user:secret@10.0.0.1:5555",
		State:  "active",
		Dst:    "https://user:secret@example.com/path",
		Domain: "user:secret@example.com",
		Port:   443,
		Reason: "bad\nvalue\twith\rcontrols",
	}
	line := FormatAccessRecord(rec)
	if strings.Contains(line, "secret") || strings.Contains(line, "user:") {
		t.Fatalf("credentials leaked into access line: %q", line)
	}
	if !strings.Contains(line, "https://example.com/path") {
		t.Fatalf("URL userinfo was not stripped: %q", line)
	}
	if !strings.Contains(line, "example.com:443") {
		t.Fatalf("domain:port missing: %q", line)
	}
	if strings.Count(line, "\n") != 1 || strings.Count(line, "\t") != 10 {
		t.Fatalf("control characters were not sanitized: %q", line)
	}
}

func TestAccessLoggerConcurrent(t *testing.T) {
	var buf bytes.Buffer
	logger := newAccessLogger(&buf, nil)

	const goroutines = 16
	const perGoroutine = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				rec := contract.AccessRecord{
					Time:   fixedTime(),
					Src:    fmt.Sprintf("10.0.0.%d:5555", g),
					State:  "active",
					Dst:    fmt.Sprintf("1.2.3.%d:443", i),
					Domain: "example.com",
					Port:   443,
				}
				if err := logger.Log(rec); err != nil {
					t.Errorf("Log: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	want := goroutines * perGoroutine
	if len(lines) != want {
		t.Fatalf("got %d lines, want %d", len(lines), want)
	}
	for _, line := range lines {
		if strings.Count(line, "\t") != 4 {
			t.Fatalf("malformed interleaved line: %q", line)
		}
	}
}

func TestLoggingWritesSystemAndAccessFiles(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	logger, err := New(Options{
		Dir:           dir,
		Level:         "debug",
		Access:        true,
		RetentionDays: 1,
		MaxSizeMB:     1,
		Stderr:        &stderr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.System().Info("hello", "component", "test")
	logger.System().Debug("debug-line")
	rec := contract.AccessRecord{
		Time:   fixedTime(),
		Src:    "10.0.0.1:5555",
		State:  "active",
		Dst:    "1.2.3.4:443",
		Domain: "example.com",
		Port:   443,
	}
	if err := logger.Access(rec); err != nil {
		t.Fatalf("Access: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	system, err := os.ReadFile(filepath.Join(dir, "dissident.log"))
	if err != nil {
		t.Fatalf("read system log: %v", err)
	}
	for _, want := range []string{"msg=hello", "component=test", "debug-line"} {
		if !strings.Contains(string(system), want) {
			t.Errorf("system log does not contain %q:\n%s", want, system)
		}
	}
	if !strings.Contains(stderr.String(), "hello") {
		t.Errorf("stderr does not contain the system log line: %q", stderr.String())
	}

	access, err := os.ReadFile(filepath.Join(dir, "access.log"))
	if err != nil {
		t.Fatalf("read access log: %v", err)
	}
	if got, want := string(access), FormatAccessRecord(rec); got != want {
		t.Errorf("access log = %q, want %q", got, want)
	}

	if err := logger.Access(rec); err == nil {
		t.Error("Access after Close should fail")
	}
}

func TestSetLevel(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(Options{Dir: dir, Level: "info", Stderr: io.Discard})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer logger.Close()

	if logger.Level() != 0 {
		t.Fatalf("initial level = %v", logger.Level())
	}
	if err := logger.SetLevel("debug"); err != nil {
		t.Fatalf("SetLevel: %v", err)
	}
	if logger.Level() != slog.LevelDebug {
		t.Fatalf("level = %v, want DEBUG", logger.Level())
	}
	if err := logger.SetLevel("loud"); err == nil {
		t.Fatal("SetLevel accepted an invalid level")
	}
}

func TestRotatingFileRotation(t *testing.T) {
	dir := t.TempDir()
	file, err := OpenRotating(RotationOptions{
		Dir:        dir,
		BaseName:   "dissident.log",
		MaxSize:    200,
		Retention:  24 * time.Hour,
		MaxBackups: 100,
	})
	if err != nil {
		t.Fatalf("OpenRotating: %v", err)
	}
	const lines = 120
	for i := 0; i < lines; i++ {
		if _, err := fmt.Fprintf(file, "line-%03d-payload\n", i); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archives, err := filepath.Glob(filepath.Join(dir, "dissident.log.*.gz"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(archives) == 0 {
		t.Fatal("expected at least one gzip archive after rotation")
	}

	all := readFileString(t, filepath.Join(dir, "dissident.log"))
	for _, archive := range archives {
		all += readGzipString(t, archive)
	}
	for i := 0; i < lines; i++ {
		marker := fmt.Sprintf("line-%03d-payload", i)
		if !strings.Contains(all, marker) {
			t.Fatalf("log line %q lost across rotation", marker)
		}
	}
}

func TestRotatingFileRetentionCleanup(t *testing.T) {
	dir := t.TempDir()
	options := RotationOptions{
		Dir:        dir,
		BaseName:   "dissident.log",
		MaxSize:    64,
		Retention:  time.Hour,
		MaxBackups: 100,
	}
	file, err := OpenRotating(options)
	if err != nil {
		t.Fatalf("OpenRotating: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := fmt.Fprintf(file, "line-%d\n", i); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	old := filepath.Join(dir, "dissident.log.20000101T000000.000000000Z.gz")
	if err := os.WriteFile(old, []byte("old archive"), 0o600); err != nil {
		t.Fatalf("write old archive: %v", err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	file, err = OpenRotating(options)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer file.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired archive still exists (err=%v)", err)
	}
}

func TestRotatingFileMaxBackups(t *testing.T) {
	dir := t.TempDir()
	file, err := OpenRotating(RotationOptions{
		Dir:        dir,
		BaseName:   "dissident.log",
		MaxSize:    64,
		Retention:  24 * time.Hour,
		MaxBackups: 2,
	})
	if err != nil {
		t.Fatalf("OpenRotating: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := fmt.Fprintf(file, "line-%d-payload\n", i); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(dir, "dissident.log.*.gz"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(archives) > 2 {
		t.Fatalf("kept %d archives, want at most 2", len(archives))
	}
	if len(archives) == 0 {
		t.Fatal("expected at least one archive")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readGzipString(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader %s: %v", path, err)
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip %s: %v", path, err)
	}
	return string(data)
}
