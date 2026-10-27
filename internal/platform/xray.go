// Package platform holds the process, network, and filesystem primitives
// shared by dissident modules: ephemeral ports, xray child processes, a
// minimal SOCKS5 dialer, HTTP CONNECT egress dialing, and egress-aware config
// rendering. It depends only on the standard library plus the contract and
// xraycfg packages.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultReadyTimeout bounds startup when the caller supplies no deadline.
const defaultReadyTimeout = 10 * time.Second

// stopGrace bounds SIGTERM handling in Stop.
const stopGrace = 5 * time.Second

// reapTimeout bounds waiting for the wait goroutine after SIGKILL.
const reapTimeout = 2 * time.Second

// xrayOutputLimit is the tail of child output retained for error messages.
const xrayOutputLimit = 32 << 10

// Xray is one running xray child process listening with a SOCKS5 inbound on
// 127.0.0.1:Port. ConfigPath is the 0600 temporary config file handed to the
// child; it is removed when the child exits or Stop is called.
type Xray struct {
	Port       int
	ConfigPath string
	Exited     <-chan struct{}

	cmd        *exec.Cmd
	done       chan struct{}
	pid        int
	output     *tailBuffer
	stopOnce   sync.Once
	stopErr    error
	closeOnce  sync.Once
	configPath string
}

// StartXray writes cfg to a fresh 0600 file in dir (os.TempDir when dir is
// empty), starts "bin -c file", and waits until the SOCKS inbound port already
// present in cfg accepts a TCP connection on 127.0.0.1. Callers render with a
// port obtained from FreePort; StartXray never rewrites the config.
//
// The child's stdout and stderr are captured into a bounded tail buffer. A
// single goroutine reaps the child and closes Exited exactly once. On any
// startup failure the child is killed and the config file removed.
func StartXray(ctx context.Context, bin string, cfg []byte, dir string) (*Xray, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(bin) == "" {
		return nil, errors.New("platform: start xray: empty binary path")
	}
	if len(cfg) == 0 {
		return nil, errors.New("platform: start xray: empty config")
	}
	port, err := configInboundPort(cfg)
	if err != nil {
		return nil, fmt.Errorf("platform: start xray: %w", err)
	}
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("platform: start xray: create config dir: %w", err)
	}

	file, err := os.CreateTemp(dir, "dissident-xray-*.json")
	if err != nil {
		return nil, fmt.Errorf("platform: start xray: create temp config: %w", err)
	}
	name := file.Name()
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		os.Remove(name)
		return nil, fmt.Errorf("platform: start xray: chmod temp config: %w", err)
	}
	if _, err := file.Write(cfg); err != nil {
		file.Close()
		os.Remove(name)
		return nil, fmt.Errorf("platform: start xray: write temp config: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return nil, fmt.Errorf("platform: start xray: close temp config: %w", err)
	}

	output := &tailBuffer{limit: xrayOutputLimit}
	cmd := exec.Command(bin, "-c", name)
	cmd.Stdout = output
	cmd.Stderr = output
	// Bound Wait if a descendant inherited the output pipes: after the child
	// exits, Wait closes them and Exited can close instead of hanging.
	cmd.WaitDelay = reapTimeout
	setChildAttrs(cmd)

	if err := cmd.Start(); err != nil {
		os.Remove(name)
		return nil, fmt.Errorf("platform: start xray: %w", err)
	}

	x := &Xray{
		Port:       port,
		ConfigPath: name,
		cmd:        cmd,
		done:       make(chan struct{}),
		pid:        cmd.Process.Pid,
		output:     output,
		configPath: name,
	}
	x.Exited = x.done

	go x.wait()

	readyCtx := ctx
	var cancel context.CancelFunc
	if _, ok := ctx.Deadline(); !ok {
		readyCtx, cancel = context.WithTimeout(ctx, defaultReadyTimeout)
		defer cancel()
	}
	if err := waitListening(readyCtx, port, x.done); err != nil {
		tail := strings.TrimSpace(output.String())
		x.kill()
		os.Remove(name)
		if tail != "" {
			return nil, fmt.Errorf("platform: start xray: did not listen on 127.0.0.1:%d: %w (output: %s)", port, err, tail)
		}
		return nil, fmt.Errorf("platform: start xray: did not listen on 127.0.0.1:%d: %w", port, err)
	}
	return x, nil
}

// wait reaps the child, records its exit, and closes Exited exactly once. The
// config file is removed as soon as the process is gone so an unexpected
// death cannot leak it.
func (x *Xray) wait() {
	_ = x.cmd.Wait()
	os.Remove(x.configPath)
	x.closeOnce.Do(func() { close(x.done) })
}

// Alive reports whether the child is still running. It is safe on a nil Xray.
func (x *Xray) Alive() bool {
	if x == nil || x.done == nil {
		return false
	}
	select {
	case <-x.done:
		return false
	default:
		return true
	}
}

// PID returns the child process id while it is alive and zero afterwards. It
// is safe on a nil Xray.
func (x *Xray) PID() int {
	if !x.Alive() {
		return 0
	}
	return x.pid
}

// Stop terminates the child gracefully on Unix (SIGTERM, then SIGKILL after
// five seconds), or immediately with Kill on Windows, reaps it, and removes
// the config file. Stop is idempotent and safe on a nil Xray.
func (x *Xray) Stop(ctx context.Context) error {
	if x == nil {
		return nil
	}
	x.stopOnce.Do(func() {
		x.stopErr = x.stop(ctx)
	})
	return x.stopErr
}

func (x *Xray) stop(ctx context.Context) error {
	defer os.Remove(x.configPath)
	if ctx == nil {
		ctx = context.Background()
	}
	if !x.Alive() {
		return nil
	}
	if x.cmd.Process != nil {
		_ = terminateProcess(x.cmd.Process)
	}
	if x.waitDone(stopGrace, ctx) {
		return nil
	}
	if x.cmd.Process != nil {
		if err := x.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("platform: kill xray pid %d: %w", x.pid, err)
		}
	}
	if !x.waitDone(reapTimeout, context.Background()) {
		return fmt.Errorf("platform: xray pid %d did not exit after SIGKILL", x.pid)
	}
	return nil
}

// kill is the faster startup-failure path: no grace period.
func (x *Xray) kill() {
	if x == nil {
		return
	}
	if x.cmd != nil && x.cmd.Process != nil {
		_ = x.cmd.Process.Kill()
	}
	x.waitDone(reapTimeout, context.Background())
	os.Remove(x.configPath)
}

// waitDone waits for the child to be reaped, the timeout, or ctx expiry.
func (x *Xray) waitDone(timeout time.Duration, ctx context.Context) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-x.done:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// configInboundPort extracts the SOCKS inbound port from an xray config.
// A SOCKS inbound wins over any other inbound; otherwise the first inbound
// with a valid port is used.
func configInboundPort(cfg []byte) (int, error) {
	var document struct {
		Inbounds []struct {
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(cfg, &document); err != nil {
		return 0, fmt.Errorf("parse config: %w", err)
	}
	fallback := 0
	for _, inbound := range document.Inbounds {
		if inbound.Port < 1 || inbound.Port > 65535 {
			continue
		}
		if strings.EqualFold(inbound.Protocol, "socks") {
			return inbound.Port, nil
		}
		if fallback == 0 {
			fallback = inbound.Port
		}
	}
	if fallback != 0 {
		return fallback, nil
	}
	return 0, errors.New("config has no inbound with a valid port")
}

// waitListening polls 127.0.0.1:port until a TCP connection succeeds.
func waitListening(ctx context.Context, port int, done <-chan struct{}) error {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-done:
			return errors.New("process exited before listening")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// FreePort reserves an ephemeral loopback port and releases it. There is an
// inherent race between FreePort returning and the caller binding, which is
// acceptable for short-lived qualification children.
func FreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("platform: allocate free port: %w", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("platform: allocate free port: unexpected address %q", listener.Addr())
	}
	return addr.Port, nil
}

// tailBuffer is a concurrency-safe writer that retains only the last limit
// bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		trimmed := make([]byte, b.limit)
		copy(trimmed, b.buf[len(b.buf)-b.limit:])
		b.buf = trimmed
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
