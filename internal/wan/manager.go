// Package wan owns the single active WAN link, the ready standby list, the
// persistence of both, and the make-before-break replacement procedure.
package wan

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/platform"
	"dissident/internal/proxy"
	"dissident/internal/qualify"
	"dissident/internal/xraycfg"
)

const (
	// passiveFailureThreshold is the number of non-nil Report outcomes for the
	// active route that triggers one asynchronous replacement.
	passiveFailureThreshold = 3
	// defaultProbeTimeout bounds one liveness probe when the configured
	// liveness interval is unusable.
	defaultProbeTimeout = 5 * time.Second
	// defaultStopTimeout bounds process shutdown during Stop and drain.
	defaultStopTimeout = 5 * time.Second
	// socksDialTimeout is the dial timeout handed to the SOCKS5 dialer.
	socksDialTimeout = 10 * time.Second
	// routeIDLength is the length of the short hash exposed as a route ID.
	routeIDLength = 12
	// replaceCooldown prevents a dead active WAN with no ready candidate from
	// scheduling a replacement on every liveness tick (and spamming the log).
	replaceCooldown = 5 * time.Minute
	// speedLivenessInterval is how often the active route is checked for the
	// configured minimum download rate. Small GET probes do not detect a WAN
	// that has collapsed from 4 Mbps to 0.6 Mbps.
	speedLivenessInterval = 5 * time.Minute
	// speedLivenessTimeout bounds the 1 MB speed probe.
	speedLivenessTimeout = 30 * time.Second
)

var (
	errNoActive       = errors.New("wan: no active route")
	errAlreadyCycling = errors.New("wan: cycle already running")
	errStopped        = errors.New("wan: manager stopped")
)

// process is the small surface the manager needs from a running xray child.
// platform.Xray is adapted by platformProcess; tests provide their own fakes.
type process interface {
	Port() int
	PID() int
	Alive() bool
	Stop(context.Context) error
}

// platformProcess adapts *platform.Xray (whose Port is a field) to process.
type platformProcess struct {
	x *platform.Xray
}

func (p *platformProcess) Port() int {
	if p == nil || p.x == nil {
		return 0
	}
	return p.x.Port
}

func (p *platformProcess) PID() int {
	if p == nil || p.x == nil {
		return 0
	}
	return p.x.PID()
}

func (p *platformProcess) Alive() bool {
	if p == nil || p.x == nil {
		return false
	}
	return p.x.Alive()
}

func (p *platformProcess) Stop(ctx context.Context) error {
	if p == nil || p.x == nil {
		return nil
	}
	return p.x.Stop(ctx)
}

// activeWAN ties the persisted metadata to the live process and route ID.
type activeWAN struct {
	meta    contract.Active
	proc    process
	routeID string
}

// Options configures a Manager. Catalog and Config are required in production;
// the zero value is useful for tests that inject the unexported function
// fields after New.
type Options struct {
	Catalog   *catalog.Store
	Tester    *qualify.Tester
	Config    *config.Manager
	StateFile string
	Logger    *slog.Logger
}

// Manager owns exactly one active xray process plus the ready metadata list.
//
// Concurrency contract:
//   - m.mu protects active, ready, failure windows, and lifecycle flags.
//   - replaceMu serializes every promotion (Replace and initial cycle
//     promotion), so no second active can be installed concurrently.
//   - cycling is an atomic guard for RunCycle; CancelCycle cancels the current
//     cycle through cycleCancel.
//   - function fields default to Tester/platform and are replaced by fakes in
//     tests to keep the package hermetic.
type Manager struct {
	catalog   *catalog.Store
	tester    *qualify.Tester
	config    *config.Manager
	logger    *slog.Logger
	stateFile string

	mu            sync.Mutex
	saveMu        sync.Mutex
	subsMu        sync.Mutex
	active        *activeWAN
	ready         []contract.Ready
	lastCycle     time.Time
	nextCycle     time.Time
	lastError     string
	cancelRequest bool
	failures      map[string][]time.Time
	probeFails    int
	nextReplaceAt time.Time
	cycleCancel   context.CancelFunc
	launchCtx     context.Context
	cancel        context.CancelFunc
	started       bool
	stopping      bool
	stopped       bool
	draining      map[int]process
	nextDrainID   int
	probeTokens   float64
	probeTokensAt time.Time

	cycling   atomic.Bool
	replaceMu sync.Mutex
	// replaceScheduled collapses bursts of passive/probe failures into one
	// asynchronous replacement attempt.
	replaceScheduled atomic.Bool
	refillScheduled  atomic.Bool

	loopsWG sync.WaitGroup
	taskWG  sync.WaitGroup

	// Function fields exist so Replace, liveness, restore fallback, and
	// persistence can be exercised without a real xray process or network.
	startPromoted func(context.Context, contract.Link) (process, error)
	startVerified func(context.Context, contract.Link) (process, error)
	stopProcess   func(context.Context, process) error
	probe         func(context.Context, string) error
	testOne       func(context.Context, contract.Link) contract.Result
	dial          func(context.Context, string, string, time.Duration) (net.Conn, error)
	egressDial    func(string, time.Duration) (func(context.Context, string, string) (net.Conn, error), error)
	now           func() time.Time
}

// New returns a Manager configured by opts. It performs no I/O and does not
// start a process; call Start.
func New(opts Options) *Manager {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cfg := opts.Config
	if cfg == nil {
		// Defaults are always valid; this only makes New robust for callers
		// that construct a manager with no config manager at all.
		cfg, _ = config.NewManager(config.Defaults())
	}
	store := opts.Catalog
	if store == nil {
		store = catalog.New()
	}
	m := &Manager{
		catalog:   store,
		tester:    opts.Tester,
		config:    cfg,
		logger:    logger,
		stateFile: opts.StateFile,
		failures:  make(map[string][]time.Time),
		now:       time.Now,
	}
	if m.stateFile == "" && cfg != nil {
		if cur := cfg.Current(); cur != nil {
			m.stateFile = cur.Config.Paths.StateFile
		}
	}
	m.startPromoted = m.startWithTester
	m.startVerified = m.verificationRestore
	m.stopProcess = func(ctx context.Context, p process) error { return p.Stop(ctx) }
	m.probe = m.probeWithTester
	m.testOne = m.testWithTester
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: timeout}
		return dialer.DialContext(ctx, network, address)
	}
	m.egressDial = platform.NewEgressDialer
	return m
}

func (m *Manager) currentConfig() config.Config {
	if m.config != nil {
		if cur := m.config.Current(); cur != nil {
			return cur.Config
		}
	}
	return config.Defaults()
}

func (m *Manager) generation() uint64 {
	if m.config != nil {
		if cur := m.config.Current(); cur != nil {
			return cur.Generation
		}
	}
	return 0
}

func (m *Manager) startWithTester(ctx context.Context, link contract.Link) (process, error) {
	if m.tester == nil {
		return nil, errors.New("wan: qualify tester is not configured")
	}
	if _, x, err := m.tester.Promote(ctx, link); err != nil {
		if x != nil {
			_ = x.Stop(context.Background())
		}
		return nil, err
	} else if x == nil {
		return nil, errors.New("wan: promote returned no xray process")
	} else {
		return &platformProcess{x: x}, nil
	}
}

func (m *Manager) probeWithTester(ctx context.Context, socksAddr string) error {
	if m.tester == nil {
		return errors.New("wan: qualify tester is not configured")
	}
	return m.tester.Probe(ctx, socksAddr)
}

func (m *Manager) testWithTester(ctx context.Context, link contract.Link) contract.Result {
	res := contract.Result{Hash: link.Hash, Stage: contract.StageReal, At: m.now().UTC()}
	if m.tester == nil {
		res.Reason = "wan: qualify tester is not configured"
		return res
	}
	return m.tester.Test(ctx, link)
}

// Name implements the app Module interface.
func (m *Manager) Name() string { return "wan" }

// Reload implements the app Module interface. The manager reads every runtime
// setting through the shared config.Manager, so there is nothing to apply; the
// reload is accepted unless the snapshot is missing.
func (m *Manager) Reload(s *config.Snapshot) error {
	if s == nil {
		return errors.New("wan: nil config snapshot")
	}
	return nil
}

// Start loads persisted state, validates links, tries to re-promote the
// persisted active, and only then launches the cycle and liveness loops.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.stopped || m.stopping {
		m.mu.Unlock()
		return errStopped
	}
	if m.started {
		m.mu.Unlock()
		return errors.New("wan: manager already started")
	}
	base, cancel := context.WithCancel(ctx)
	m.launchCtx = base
	m.cancel = cancel
	m.started = true
	m.mu.Unlock()

	ps := m.loadState()
	cfg := m.currentConfig()
	target := cfg.Cycle.ReadyTarget
	ready := make([]contract.Ready, 0, len(ps.Ready))
	for _, r := range dedupeReady(ps.Ready) {
		if err := xraycfg.Supports(r.Link); err != nil {
			m.logger.Warn("wan: dropping unsupported persisted ready link", "hash", r.Link.Hash, "err", err)
			continue
		}
		ready = append(ready, r)
	}
	sortReady(ready)
	if target > 0 && len(ready) > target {
		ready = ready[:target]
	}
	m.mu.Lock()
	m.ready = ready
	m.mu.Unlock()

	var restoreErr error
	if ps.Active != nil {
		restoreErr = m.restoreActive(base, *ps.Active)
		if restoreErr == nil {
			// A stale ready copy of the now-active link must not survive.
			m.mu.Lock()
			m.removeReadyLocked(ps.Active.Link.Hash)
			m.mu.Unlock()
			_ = m.saveState()
		}
		if restoreErr != nil {
			m.logger.Warn("wan: could not restore persisted active; starting a cycle", "hash", ps.Active.Link.Hash, "err", restoreErr)
			m.mu.Lock()
			m.active = nil
			m.mu.Unlock()
			_ = m.saveState()
			m.setLastError(restoreErr.Error())
		}
	}

	// The loops are started only after the restore attempt so a probe or a
	// cycle can never observe a half-installed active. Start holds m.mu while
	// registering the loops so a concurrent Stop cannot begin Wait-ing first.
	m.mu.Lock()
	if m.stopping || m.stopped {
		a := m.active
		m.active = nil
		m.mu.Unlock()
		if a != nil && a.proc != nil {
			stopCtx, stopCancel := boundedContext(context.Background(), defaultStopTimeout)
			_ = m.stopProcess(stopCtx, a.proc)
			stopCancel()
		}
		cancel()
		return errStopped
	}
	m.loopsWG.Add(4)
	m.mu.Unlock()
	go m.cycleLoop(base)
	go m.livenessLoop(base)
	go m.readyLivenessLoop(base)
	go m.speedLivenessLoop(base)

	if !m.hasActive() {
		m.triggerCycle("startup: no active WAN")
	} else {
		m.maybeTriggerRefill("startup: ready below minimum")
	}
	return nil
}

func (m *Manager) restoreActive(ctx context.Context, pa contract.Active) error {
	if err := xraycfg.Supports(pa.Link); err != nil {
		return fmt.Errorf("unsupported active link: %w", err)
	}
	timeout := m.currentConfig().Swap.PromotionTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	p, err := m.promotedOrVerified(ctx, pa.Link, timeout)
	if err != nil {
		return fmt.Errorf("wan: restore active: %w", err)
	}
	if p == nil || p.Port() <= 0 {
		if p != nil {
			_ = m.stopProcess(context.Background(), p)
		}
		return errors.New("wan: restored active process has no local port")
	}
	since := pa.Since
	if since.IsZero() {
		since = m.now().UTC()
	}
	meta := contract.Active{
		Link:      cloneLink(pa.Link),
		Since:     since,
		ExitIP:    pa.ExitIP,
		XrayPID:   p.PID(),
		LocalPort: p.Port(),
	}
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		_ = m.stopProcess(context.Background(), p)
		return errStopped
	}
	m.active = &activeWAN{meta: meta, proc: p, routeID: routeIDFor(pa.Link.Hash, p.PID())}
	m.probeFails = 0
	m.nextReplaceAt = time.Time{}
	m.failures = make(map[string][]time.Time)
	m.mu.Unlock()
	_ = m.saveState()
	m.logger.Info("wan: restored active", "hash", pa.Link.Hash, "port", p.Port(), "pid", p.PID())
	return nil
}

// restoreAttempt runs one bounded start function and stops any partially
// started process on error.
func (m *Manager) restoreAttempt(ctx context.Context, link contract.Link, timeout time.Duration,
	start func(context.Context, contract.Link) (process, error)) (process, error) {
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := start(actx, link)
	if err != nil {
		if p != nil {
			stopCtx, stopCancel := boundedContext(context.Background(), defaultStopTimeout)
			_ = m.stopProcess(stopCtx, p)
			stopCancel()
		}
		return nil, err
	}
	return p, nil
}

// promotedOrVerified performs one full promotion attempt and, if it fails for
// a reason other than cancellation, one verification-only start. Ready
// candidates already passed the full pipeline, so a transient speed-endpoint
// failure must not cost them their place. Each attempt gets its own bounded
// timeout; a failed attempt's process is stopped.
func (m *Manager) promotedOrVerified(ctx context.Context, link contract.Link, timeout time.Duration) (process, error) {
	p, promoteErr := m.restoreAttempt(ctx, link, timeout, m.startPromoted)
	if promoteErr == nil && p != nil {
		return p, nil
	}
	if promoteErr == nil {
		promoteErr = errors.New("wan: promotion returned no process")
	}
	if ctx.Err() != nil {
		return nil, promoteErr
	}
	m.logger.Warn("wan: full promotion failed; retrying verification-only",
		"hash", link.Hash, "err", promoteErr)
	p, verifyErr := m.restoreAttempt(ctx, link, timeout, m.startVerified)
	if verifyErr != nil {
		return nil, fmt.Errorf("wan: promotion failed (%v); verification fallback failed: %w", promoteErr, verifyErr)
	}
	if p == nil {
		return nil, errors.New("wan: verification fallback returned no process")
	}
	if m.tester != nil && p.Port() > 0 {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Port()))
		if err := m.tester.Upload(ctx, addr); err != nil {
			stopCtx, stopCancel := boundedContext(context.Background(), defaultStopTimeout)
			_ = m.stopProcess(stopCtx, p)
			stopCancel()
			return nil, fmt.Errorf("wan: verification upload failed: %w", err)
		}
		if err := m.tester.HTTP2(ctx, addr); err != nil {
			stopCtx, stopCancel := boundedContext(context.Background(), defaultStopTimeout)
			_ = m.stopProcess(stopCtx, p)
			stopCancel()
			return nil, fmt.Errorf("wan: verification HTTP/2 failed: %w", err)
		}
	}
	m.logger.Info("wan: candidate started via verification-only fallback",
		"hash", link.Hash, "port", p.Port())
	return p, nil
}

// Stop cancels the loops, waits for them (bounded by ctx), stops the active
// xray process (bounded), and persists the final state.
func (m *Manager) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	cancel := m.cancel
	cycleCancel := m.cycleCancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cycleCancel != nil {
		cycleCancel()
	}

	done := make(chan struct{})
	go func() {
		m.loopsWG.Wait()
		m.taskWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		m.logger.Warn("wan: stop wait timed out", "err", ctx.Err())
	}

	m.mu.Lock()
	a := m.active
	m.active = nil
	m.ready = cloneReady(m.ready)
	m.mu.Unlock()

	var stopErr error
	if a != nil && a.proc != nil {
		stopCtx, stopCancel := boundedContext(ctx, defaultStopTimeout)
		stopErr = m.stopProcess(stopCtx, a.proc)
		stopCancel()
		if stopErr != nil {
			m.logger.Warn("wan: stopping active xray failed", "err", stopErr)
		}
	}

	// KEEP the persisted active metadata for fast restart even though the
	// process is gone. Start re-promotes it and derives a fresh PID/port.
	if a != nil {
		m.mu.Lock()
		a.proc = nil
		a.meta.XrayPID = 0
		a.meta.LocalPort = 0
		m.active = a
		m.mu.Unlock()
	}
	saveErr := m.saveState()
	m.mu.Lock()
	m.active = nil
	m.stopping = false
	m.stopped = true
	m.started = false
	m.ready = nil
	m.cycleCancel = nil
	m.mu.Unlock()

	if stopErr != nil {
		return fmt.Errorf("wan: stop active: %w", stopErr)
	}
	if saveErr != nil {
		return saveErr
	}
	return nil
}

func boundedContext(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, fallback)
}

func (m *Manager) hasActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil && m.active.proc != nil
}

func (m *Manager) readyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ready)
}

func (m *Manager) setLastError(msg string) {
	m.mu.Lock()
	m.lastError = msg
	m.mu.Unlock()
}

// Snapshot returns an immutable copy of the runtime view.
func (m *Manager) Snapshot() contract.Snapshot {
	m.mu.Lock()
	var active *contract.Active
	if m.active != nil {
		meta := m.active.meta
		meta.Link = cloneLink(meta.Link)
		active = &meta
	}
	ready := cloneReady(m.ready)
	m.mu.Unlock()

	total, supported, testing := 0, 0, 0
	if m.catalog != nil {
		total, supported, testing = m.catalog.Stats()
	}
	return contract.Snapshot{
		Generation:     m.generation(),
		Active:         active,
		Ready:          ready,
		CatalogCount:   total,
		SupportedCount: supported,
		Testing:        testing,
		Updated:        m.now().UTC(),
	}
}

// Entries returns a deep copy of the catalog view.
func (m *Manager) Entries() []contract.Entry {
	if m.catalog == nil {
		return nil
	}
	return copyEntries(m.catalog.Entries())
}

// Route returns the current active SOCKS5 route. There is deliberately no
// fallback: with no active WAN the proxy must fail closed.
func (m *Manager) Route(ctx context.Context) (proxy.Route, error) {
	_ = ctx
	m.mu.Lock()
	a := m.active
	if a == nil || a.proc == nil {
		m.mu.Unlock()
		return proxy.Route{}, errNoActive
	}
	port := a.meta.LocalPort
	if port <= 0 {
		port = a.proc.Port()
	}
	id := a.routeID
	m.mu.Unlock()
	if port <= 0 {
		return proxy.Route{}, errors.New("wan: active WAN has no local SOCKS port")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return proxy.Route{
		ID:   id,
		Dial: &platform.SOCKS5Dialer{Addr: addr, Timeout: socksDialTimeout},
	}, nil
}

// Report records the outcome of one proxied connection. It never blocks: a
// third passive failure inside probe_window schedules one asynchronous
// replacement. Successful routes reset their failure counter.
func (m *Manager) Report(ctx context.Context, route proxy.Route, err error) {
	_ = ctx
	if err == nil {
		m.mu.Lock()
		if route.ID != "" {
			delete(m.failures, route.ID)
		}
		m.mu.Unlock()
		return
	}

	window := m.currentConfig().Cycle.ProbeWindow
	if window <= 0 {
		window = 8 * time.Second
	}
	now := m.now()

	m.mu.Lock()
	a := m.active
	if a == nil || a.routeID == "" || route.ID != a.routeID {
		m.mu.Unlock()
		return
	}
	times := append(m.failures[route.ID], now)
	cut := now.Add(-window)
	trimmed := times[:0]
	for _, t := range times {
		if !t.Before(cut) {
			trimmed = append(trimmed, t)
		}
	}
	m.failures[route.ID] = trimmed
	n := len(trimmed)
	m.mu.Unlock()

	if n >= passiveFailureThreshold {
		m.logger.Warn("wan: passive failures exceeded threshold; replacing", "route", route.ID, "failures", n)
		m.triggerReplace("passive failures")
	}
}

// Cycling reports whether a cycle is currently running.
func (m *Manager) Cycling() bool { return m.cycling.Load() }

// LastCycle returns when the last cycle finished.
func (m *Manager) LastCycle() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCycle
}

// NextCycle returns the scheduled start of the next periodic cycle.
func (m *Manager) NextCycle() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextCycle
}

// LastError returns the last cycle/promotion error message, or "".
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

// CancelCycle cancels the currently running cycle, if any.
func (m *Manager) CancelCycle() {
	m.mu.Lock()
	cancel := m.cycleCancel
	if cancel == nil && m.cycling.Load() {
		// RunCycle won the CAS but has not published its cancel function yet.
		m.cancelRequest = true
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) triggerCycle(reason string) {
	m.launchTask(reason, func(ctx context.Context) {
		if err := m.RunCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("wan: triggered cycle failed", "reason", reason, "err", err)
		}
	})
}

// launchTask starts a bounded background task unless the manager is shutting
// down. It returns false when the task was refused.
func (m *Manager) launchTask(reason string, fn func(context.Context)) bool {
	m.mu.Lock()
	if m.stopping || m.stopped {
		m.mu.Unlock()
		return false
	}
	ctx := m.launchCtx
	if ctx == nil {
		ctx = context.Background()
	}
	m.taskWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.taskWG.Done()
		defer func() {
			if rec := recover(); rec != nil {
				m.logger.Error("wan: background task panic", "reason", reason, "panic", rec)
			}
		}()
		fn(ctx)
	}()
	return true
}

func (m *Manager) triggerReplace(reason string) {
	if !m.replaceScheduled.CompareAndSwap(false, true) {
		return
	}
	budget := m.replaceBudget()
	ok := m.launchTask(reason, func(ctx context.Context) {
		defer m.replaceScheduled.Store(false)
		rctx, cancel := context.WithTimeout(ctx, budget)
		defer cancel()
		if err := m.Replace(rctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("wan: asynchronous replacement failed", "reason", reason, "err", err)
		}
	})
	if !ok {
		m.replaceScheduled.Store(false)
	}
}

func (m *Manager) replaceBudget() time.Duration {
	cfg := m.currentConfig()
	promo := cfg.Swap.PromotionTimeout
	if promo <= 0 {
		promo = 15 * time.Second
	}
	m.mu.Lock()
	n := len(m.ready)
	m.mu.Unlock()
	if n < 1 {
		n = 1
	}
	return time.Duration(n)*promo + 5*time.Second
}

func (m *Manager) cycleLoop(ctx context.Context) {
	defer m.loopsWG.Done()
	interval := m.currentConfig().Cycle.Interval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.RunCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
				m.logger.Warn("wan: periodic cycle failed", "err", err)
			}
		}
	}
}

func (m *Manager) livenessLoop(ctx context.Context) {
	defer m.loopsWG.Done()
	interval := m.currentConfig().Cycle.ActiveLiveness
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeOnce(ctx)
		}
	}
}

func (m *Manager) speedLivenessLoop(ctx context.Context) {
	defer m.loopsWG.Done()
	ticker := time.NewTicker(speedLivenessInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.speedProbeOnce(ctx)
		}
	}
}

// speedProbeOnce verifies that the active route still meets the configured
// minimum download rate. A WAN that passes the small liveness GET but has
// collapsed to sub-1 Mbps is not useful for browsing and must be replaced.
func (m *Manager) speedProbeOnce(ctx context.Context) {
	if m.tester == nil {
		return
	}
	cfg := m.currentConfig()
	minimum := cfg.Test.MinSpeedMbps
	if minimum <= 0 {
		return
	}
	now := m.now()

	m.mu.Lock()
	a := m.active
	if a == nil || a.proc == nil || now.Before(m.nextReplaceAt) {
		m.mu.Unlock()
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.meta.LocalPort))
	if a.meta.LocalPort <= 0 {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(a.proc.Port()))
	}
	routeID := a.routeID
	m.nextReplaceAt = now.Add(replaceCooldown)
	m.mu.Unlock()

	pctx, cancel := context.WithTimeout(ctx, speedLivenessTimeout)
	speed, _, err := m.tester.Speed(pctx, addr)
	cancel()
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	if m.active == nil || m.active.routeID != routeID {
		m.mu.Unlock()
		return
	}
	if err == nil && speed >= minimum {
		m.nextReplaceAt = time.Time{}
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.logger.Warn("wan: active speed below minimum; replacing",
		"route", routeID, "mbps", speed, "minimum", minimum, "err", err)
	m.triggerReplace("speed liveness")
}

// probeOnce runs at most one active liveness probe, respecting the global
// per-minute budget. Three consecutive failures schedule a replacement; a
// success resets the counter. Counting consecutive probes avoids a window
// shorter than the probe interval, which could never reach the threshold.
func (m *Manager) probeOnce(ctx context.Context) {
	cfg := m.currentConfig()
	now := m.now()

	m.mu.Lock()
	a := m.active
	if a == nil || a.proc == nil {
		m.mu.Unlock()
		return
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.meta.LocalPort))
	if a.meta.LocalPort <= 0 {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(a.proc.Port()))
	}
	routeID := a.routeID
	if !m.takeProbeTokenLocked(now, cfg.Cycle.ProbeBudgetPerMin) {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	timeout := cfg.Cycle.ActiveLiveness
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	err := m.probe(pctx, addr)
	cancel()
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	if m.active == nil || m.active.routeID != routeID {
		m.mu.Unlock()
		return
	}
	if err != nil {
		m.probeFails++
		threshold := cfg.Cycle.ProbeCount
		if threshold < 1 {
			threshold = 3
		}
		failures := m.probeFails
		if failures < threshold {
			m.mu.Unlock()
			m.logger.Debug("wan: liveness probe failed", "route", routeID, "err", err, "failures", failures)
			return
		}
		m.probeFails = 0
		if now.Before(m.nextReplaceAt) {
			m.mu.Unlock()
			m.logger.Debug("wan: liveness replacement cooling down", "route", routeID, "next", m.nextReplaceAt)
			return
		}
		m.nextReplaceAt = now.Add(replaceCooldown)
		m.mu.Unlock()
		m.logger.Warn("wan: active liveness failed; replacing", "route", routeID, "failures", failures, "err", err)
		m.triggerReplace("liveness")
		return
	}
	m.probeFails = 0
	m.nextReplaceAt = time.Time{}
	m.mu.Unlock()
}

// takeProbeTokenLocked implements a simple token bucket: capacity is the
// configured per-minute budget and refill is budget/60 per second. Callers
// must hold m.mu.
func (m *Manager) takeProbeTokenLocked(now time.Time, perMin int) bool {
	if perMin < 1 {
		return false
	}
	capTokens := float64(perMin)
	if m.probeTokensAt.IsZero() {
		m.probeTokens = capTokens
		m.probeTokensAt = now
	}
	elapsed := now.Sub(m.probeTokensAt).Seconds()
	if elapsed > 0 {
		m.probeTokens += elapsed * float64(perMin) / 60
		m.probeTokensAt = now
	}
	if m.probeTokens > capTokens {
		m.probeTokens = capTokens
	}
	if m.probeTokens < 1 {
		return false
	}
	m.probeTokens--
	return true
}

func routeIDFor(hash string, pid int) string {
	if hash == "" {
		if pid > 0 {
			return "pid-" + strconv.Itoa(pid)
		}
		return "active"
	}
	if len(hash) > routeIDLength {
		return hash[:routeIDLength]
	}
	return hash
}

func cloneLink(in contract.Link) contract.Link {
	out := in
	if in.Params != nil {
		out.Params = make(map[string]string, len(in.Params))
		for k, v := range in.Params {
			out.Params[k] = v
		}
	}
	return out
}

func cloneResult(in contract.Result) contract.Result { return in }

func cloneReady(in []contract.Ready) []contract.Ready {
	if in == nil {
		return nil
	}
	out := make([]contract.Ready, len(in))
	for i := range in {
		out[i] = in[i]
		out[i].Link = cloneLink(in[i].Link)
	}
	return out
}

func cloneEntry(in contract.Entry) contract.Entry {
	out := in
	out.Link = cloneLink(in.Link)
	out.Last = cloneResult(in.Last)
	return out
}

func copyEntries(in []contract.Entry) []contract.Entry {
	if in == nil {
		return nil
	}
	out := make([]contract.Entry, len(in))
	for i := range in {
		out[i] = cloneEntry(in[i])
	}
	return out
}
