package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dissident/internal/config"
)

// SupervisorOptions configures module lifecycle handling.
type SupervisorOptions struct {
	// Logger receives lifecycle and panic messages. Nil uses slog.Default.
	Logger *slog.Logger
	// StopTimeout bounds the shutdown of all modules and tracked goroutines.
	// Zero means 30 seconds.
	StopTimeout time.Duration
}

// Supervisor starts modules in registration order, reloads them
// transactionally, and stops them in reverse order. Panics in module callbacks
// or tracked goroutines are recovered and counted; they never kill the
// process.
type Supervisor struct {
	opts SupervisorOptions

	runMu sync.Mutex // serializes Start/Reload/Stop
	mu    sync.Mutex // protects the fields below

	modules        []Module
	startedModules []Module
	current        *config.Snapshot
	ctx            context.Context
	cancel         context.CancelFunc
	started        bool
	stopping       bool
	stopped        bool

	wg     sync.WaitGroup
	panics atomic.Uint64
}

// NewSupervisor returns an empty supervisor.
func NewSupervisor(opts SupervisorOptions) *Supervisor {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 30 * time.Second
	}
	return &Supervisor{opts: opts}
}

// Add registers a module. Modules may only be added before Start.
func (s *Supervisor) Add(m Module) error {
	if m == nil {
		return errors.New("app: nil module")
	}
	name := strings.TrimSpace(m.Name())
	if name == "" {
		return errors.New("app: module name is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopping {
		return fmt.Errorf("app: cannot add module %q after start", name)
	}
	for _, existing := range s.modules {
		if existing.Name() == name {
			return fmt.Errorf("app: duplicate module %q", name)
		}
	}
	s.modules = append(s.modules, m)
	return nil
}

// Modules returns the registered module names in start order.
func (s *Supervisor) Modules() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, len(s.modules))
	for i, m := range s.modules {
		names[i] = m.Name()
	}
	return names
}

// Start starts every module in registration order. If one fails, modules that
// already started are stopped in reverse order and the error is returned.
func (s *Supervisor) Start(ctx context.Context, snap *config.Snapshot) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("app: supervisor already started")
	}
	if s.stopping || s.stopped {
		s.mu.Unlock()
		return errors.New("app: supervisor was stopped and cannot be restarted")
	}
	modules := append([]Module(nil), s.modules...)
	s.ctx, s.cancel = context.WithCancel(ctx)
	if snap != nil {
		s.current = snap.Clone()
	}
	runCtx := s.ctx
	s.mu.Unlock()

	started := make([]Module, 0, len(modules))
	for _, m := range modules {
		if err := s.startModule(runCtx, m); err != nil {
			stopCtx, cancel := s.stopContext(context.Background())
			var errs []error
			errs = append(errs, err)
			for i := len(started) - 1; i >= 0; i-- {
				if serr := s.stopModule(stopCtx, started[i]); serr != nil {
					errs = append(errs, serr)
				}
			}
			cancel()
			if s.cancel != nil {
				s.cancel()
			}
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
			return errors.Join(errs...)
		}
		started = append(started, m)
	}

	s.mu.Lock()
	s.started = true
	s.startedModules = started
	s.mu.Unlock()
	return nil
}

// Reload validates the new snapshot with every module that supports
// ValidateReload, then applies it in order. If a module's Reload fails, the
// modules already reloaded are returned to the previous snapshot and the
// error is returned, leaving the supervisor's current snapshot unchanged.
func (s *Supervisor) Reload(next *config.Snapshot) error {
	if next == nil {
		return errors.New("app: nil config snapshot")
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	if !s.started || s.stopping || s.stopped {
		s.mu.Unlock()
		return errors.New("app: supervisor is not running")
	}
	modules := append([]Module(nil), s.modules...)
	previous := s.current
	s.mu.Unlock()

	next = next.Clone()

	for _, m := range modules {
		validator, ok := m.(ReloadValidator)
		if !ok {
			continue
		}
		if err := s.validateReload(m.Name(), validator, next); err != nil {
			return fmt.Errorf("app: validate reload for module %s: %w", m.Name(), err)
		}
	}

	applied := make([]Module, 0, len(modules))
	for _, m := range modules {
		if err := s.reloadModule(m, next); err != nil {
			errs := []error{err}
			if previous != nil {
				for i := len(applied) - 1; i >= 0; i-- {
					if rerr := s.reloadModule(applied[i], previous); rerr != nil {
						errs = append(errs, rerr)
					}
				}
			}
			return errors.Join(errs...)
		}
		applied = append(applied, m)
	}

	s.mu.Lock()
	s.current = next
	s.mu.Unlock()
	return nil
}

// Stop cancels tracked goroutines and stops modules in reverse order, bounded
// by ctx and StopTimeout. It is idempotent.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	modules := append([]Module(nil), s.startedModules...)
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	stopCtx, stopCancel := s.stopContext(ctx)
	defer stopCancel()

	var errs []error
	for i := len(modules) - 1; i >= 0; i-- {
		if err := s.stopModule(stopCtx, modules[i]); err != nil {
			errs = append(errs, err)
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-stopCtx.Done():
		errs = append(errs, fmt.Errorf("app: tracked goroutines did not stop: %w", stopCtx.Err()))
	}

	s.mu.Lock()
	s.started = false
	s.stopped = true
	s.startedModules = nil
	s.mu.Unlock()
	return errors.Join(errs...)
}

// Go runs fn in a tracked goroutine. A panic is recovered and logged. It
// returns false when the supervisor is not running or is shutting down; in
// that case fn is not started.
func (s *Supervisor) Go(module string, fn func(ctx context.Context)) bool {
	if fn == nil {
		return false
	}
	s.mu.Lock()
	if s.ctx == nil || s.stopping || s.stopped {
		s.mu.Unlock()
		return false
	}
	ctx := s.ctx
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logPanic(module, "goroutine", r)
			}
		}()
		fn(ctx)
	}()
	return true
}

// Current returns the current config snapshot. Treat it as read-only.
func (s *Supervisor) Current() *config.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// RecoveredPanics returns the number of module/goroutine panics recovered.
func (s *Supervisor) RecoveredPanics() uint64 {
	return s.panics.Load()
}

func (s *Supervisor) startModule(ctx context.Context, m Module) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logPanic(m.Name(), "start", r)
			err = fmt.Errorf("app: module %s panicked during start: %v", m.Name(), r)
		}
	}()
	if serr := m.Start(ctx); serr != nil {
		return fmt.Errorf("app: module %s start: %w", m.Name(), serr)
	}
	return nil
}

func (s *Supervisor) reloadModule(m Module, snap *config.Snapshot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logPanic(m.Name(), "reload", r)
			err = fmt.Errorf("app: module %s panicked during reload: %v", m.Name(), r)
		}
	}()
	if serr := m.Reload(snap); serr != nil {
		return fmt.Errorf("app: module %s reload: %w", m.Name(), serr)
	}
	return nil
}

func (s *Supervisor) validateReload(module string, v ReloadValidator, snap *config.Snapshot) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logPanic(module, "validate-reload", r)
			err = fmt.Errorf("module panicked during reload validation: %v", r)
		}
	}()
	return v.ValidateReload(snap)
}

func (s *Supervisor) stopModule(ctx context.Context, m Module) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logPanic(m.Name(), "stop", r)
			err = fmt.Errorf("app: module %s panicked during stop: %v", m.Name(), r)
		}
	}()
	if serr := m.Stop(ctx); serr != nil {
		return fmt.Errorf("app: module %s stop: %w", m.Name(), serr)
	}
	return nil
}

func (s *Supervisor) stopContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < s.opts.StopTimeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, s.opts.StopTimeout)
}

func (s *Supervisor) logPanic(module, phase string, recovered any) {
	s.panics.Add(1)
	s.opts.Logger.Error("module panic recovered",
		"module", module,
		"phase", phase,
		"panic", fmt.Sprint(recovered),
		"stack", string(debug.Stack()),
	)
}
