package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Service is a supervised child process (viberayd or viberoxy).
type Service struct {
	Name      string
	Binary    string
	EnvFile   string // env file the process reads (or empty)
	ExtraArgs []string
	// AutoRestart restarts the child after an unexpected exit. New services
	// enable this policy by default.
	AutoRestart bool

	mu             sync.Mutex
	cmd            *exec.Cmd
	started        time.Time
	restarts       int
	stderr         *ringBuffer
	lastErr        string
	lastRestartAt  time.Time
	restartBackoff time.Duration
	stopping       bool
	exitCh         chan struct{} // closed by the reaper when the process exits
}

// NewService creates a service definition.
func NewService(name, binary, envFile string, extraArgs ...string) *Service {
	return &Service{
		Name:           name,
		Binary:         binary,
		EnvFile:        envFile,
		ExtraArgs:      extraArgs,
		AutoRestart:    true,
		stderr:         newRingBuffer(50),
		restartBackoff: time.Second,
	}
}

// Start launches the child with the env file exported as its environment
// (KEY=VALUE lines). It returns once the process is spawned, not once it is
// healthy — health is the caller's job via IsRunning.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd != nil && s.cmd.Process != nil {
		return fmt.Errorf("%s already running (pid %d)", s.Name, s.cmd.Process.Pid)
	}

	cmd := exec.CommandContext(ctx, s.Binary, s.ExtraArgs...)
	cmd.Env = os.Environ()
	if s.EnvFile != "" {
		env, err := readEnvFile(s.EnvFile)
		if err != nil {
			// Missing env file is fine at first boot: the daemon starts
			// with the inherited environment (defaults apply).
			if !os.IsNotExist(err) {
				return fmt.Errorf("read env %s: %w", s.EnvFile, err)
			}
			slog.Warn("env file missing, starting with inherited env", "service", s.Name, "env_file", s.EnvFile)
		} else {
			cmd.Env = append(cmd.Env, env...)
		}
	}

	pipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", s.Name, err)
	}

	s.cmd = cmd
	s.started = time.Now()
	s.restarts++
	s.stopping = false
	s.exitCh = make(chan struct{})
	exitCh := s.exitCh

	// Tail stderr into the ring buffer.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pipe.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.stderr.Write(string(buf[:n]))
				s.mu.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					s.mu.Lock()
					s.lastErr = err.Error()
					s.mu.Unlock()
				}
				return
			}
		}
	}()

	// Reap the process and remember the exit. Only this goroutine calls
	// cmd.Wait(); Stop waits on exitCh instead.
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		if s.cmd == cmd {
			s.cmd = nil
		}
		if err != nil {
			s.lastErr = err.Error()
		}
		close(exitCh)
		shouldRestart := s.AutoRestart && !s.stopping && ctx.Err() == nil
		delay := s.restartBackoff
		if delay <= 0 {
			delay = time.Second
		}
		s.restartBackoff = min(delay*2, 30*time.Second)
		s.mu.Unlock()
		slog.Warn("service exited", "service", s.Name, "err", err)

		if !shouldRestart {
			return
		}

		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		s.mu.Lock()
		shouldRestart = s.AutoRestart && !s.stopping && s.cmd == nil
		s.mu.Unlock()
		if !shouldRestart {
			return
		}

		if err := s.Start(ctx); err != nil {
			s.mu.Lock()
			s.lastErr = err.Error()
			s.mu.Unlock()
			slog.Error("service restart failed", "service", s.Name, "err", err)
			return
		}
		s.mu.Lock()
		s.lastRestartAt = time.Now()
		s.mu.Unlock()
	}()

	slog.Info("service started", "service", s.Name, "pid", cmd.Process.Pid, "restarts", s.restarts)
	return nil
}

// IsRunning reports whether the child is currently alive.
func (s *Service) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil
}

// Restart stops (if running) and starts the child again.
func (s *Service) Restart(ctx context.Context) error {
	s.Stop()
	// Give the OS a moment to free the port.
	time.Sleep(300 * time.Millisecond)
	return s.Start(ctx)
}

// Stop terminates the child (SIGTERM, then SIGKILL after a grace period).
func (s *Service) Stop() {
	s.mu.Lock()
	cmd := s.cmd
	exitCh := s.exitCh
	s.stopping = true
	s.cmd = nil
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid
	_ = cmd.Process.Signal(os.Interrupt) // SIGINT → daemons exit cleanly
	select {
	case <-exitCh:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-exitCh
	}
	slog.Info("service stopped", "service", s.Name, "pid", pid)
}

// Status returns the current process view for the API.
type Status struct {
	Name          string    `json:"name"`
	Running       bool      `json:"running"`
	Pid           int       `json:"pid,omitempty"`
	UptimeSec     int64     `json:"uptime_sec,omitempty"`
	Restarts      int       `json:"restarts"`
	AutoRestart   bool      `json:"auto_restart"`
	LastRestartAt time.Time `json:"last_restart_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Stderr        []string  `json:"stderr_tail"`
	StartedAt     time.Time `json:"started_at,omitempty"`
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{
		Name:          s.Name,
		Running:       s.cmd != nil && s.cmd.Process != nil,
		Restarts:      s.restarts,
		AutoRestart:   s.AutoRestart,
		LastRestartAt: s.lastRestartAt,
		LastError:     s.lastErr,
		Stderr:        s.stderr.Lines(),
	}
	if st.Running {
		st.Pid = s.cmd.Process.Pid
		st.UptimeSec = int64(time.Since(s.started).Seconds())
		st.StartedAt = s.started
	}
	return st
}

// readEnvFile parses KEY=VALUE lines from an env file, returning them as
// "KEY=VALUE" entries for exec.Cmd.Env.
func readEnvFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseEnvFile(string(data)), nil
}

func parseEnvFile(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			out = append(out, line)
		}
	}
	return out
}

// ringBuffer keeps the last N lines of text.
type ringBuffer struct {
	lines []string
	max   int
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max}
}

func (r *ringBuffer) Write(s string) {
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			r.push(s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		r.push(s[start:])
	}
}

func (r *ringBuffer) push(line string) {
	if line == "" {
		return
	}
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *ringBuffer) Lines() []string {
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}
