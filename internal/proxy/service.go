// Package proxy implements dissident's data plane: one TCP listener that
// serves both HTTP CONNECT and SOCKS5 clients.
//
// A connection is sniffed by its first byte. After the front-end handshake the
// service asks its RouteProvider for the active WAN route, dials the target
// through that route, and relays bytes in both directions until both halves
// finish, an idle timeout fires, or the service is stopped.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"dissident/internal/contract"
)

// Dialer establishes a connection to a target through a WAN route.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Route is the WAN route selected for one proxied connection.
type Route struct {
	ID   string
	Dial Dialer
}

// RouteProvider selects the active route for a connection and receives its
// final outcome exactly once.
type RouteProvider interface {
	Route(ctx context.Context) (Route, error)
	Report(ctx context.Context, route Route, err error)
}

// Options configures a Service. Provider and Listen are required; Access and
// Logger may be nil.
type Options struct {
	Listen   string
	Provider RouteProvider
	Access   func(contract.AccessRecord)
	Logger   *slog.Logger
}

const (
	// defaultHandshakeTimeout bounds the client's initial request. It is
	// cleared before route selection/dialing so a slow upstream cannot make a
	// valid tunnel fail on an expired client deadline.
	defaultHandshakeTimeout = 10 * time.Second

	// defaultIdleTimeout is a relay inactivity bound, not a total-download
	// limit: an active transfer of any length is never cut. It is a field on
	// Service so tests can shorten it.
	defaultIdleTimeout = 5 * time.Minute

	// defaultMaxConnections bounds handler goroutines; accept blocks for
	// backpressure when every slot is in use.
	defaultMaxConnections = 512

	// reportTimeout bounds one Provider.Report call. Reporting uses a context
	// detached from shutdown so the outcome is still recorded while stopping.
	reportTimeout = 5 * time.Second
)

// AccessRecord.State values. They are the strings placed in
// contract.AccessRecord.State.
const (
	stateOK      = "OK"
	stateFail    = "FAIL"
	stateTimeout = "TIMEOUT"
	stateCancel  = "CANCEL"
	stateDeny    = "DENY"
)

// AccessRecord.Proto values.
const (
	protoConnect = "connect"
	protoSOCKS5  = "socks5"
)

var (
	errAlreadyStarted = errors.New("proxy: service already started")
	errServiceStopped = errors.New("proxy: service stopped")
	errRelayIdle      = errors.New("proxy: relay idle timeout")
)

// Service is the single-port HTTP CONNECT + SOCKS5 data plane.
//
// Start and Stop are independent of the request path: Start binds
// synchronously and returns once the listener is live, while accepted
// connections are handled by a bounded pool of goroutines rooted in the
// context passed to Start.
type Service struct {
	opts   Options
	logger *slog.Logger

	// handshakeTimeout, idleTimeout and maxConnections are fields rather than
	// constants so tests can drive short bounds and small pools.
	handshakeTimeout time.Duration
	idleTimeout      time.Duration
	maxConnections   int

	mu      sync.Mutex
	started bool
	stopped bool
	ln      net.Listener
	ctx     context.Context
	cancel  context.CancelFunc
	conns   map[net.Conn]struct{}
	sem     chan struct{}

	wg   sync.WaitGroup
	done chan struct{}
}

// New returns a Service configured by opts. It does not bind.
func New(opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		opts:             opts,
		logger:           logger,
		handshakeTimeout: defaultHandshakeTimeout,
		idleTimeout:      defaultIdleTimeout,
		maxConnections:   defaultMaxConnections,
		done:             make(chan struct{}),
	}
}

// Start binds the listener and starts the accept loop. It returns nil once the
// listener is live; binding errors are returned before any goroutine starts.
// The context bounds the service lifetime: cancelling it is equivalent to
// calling Stop.
func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.opts.Provider == nil {
		return errors.New("proxy: route provider is required")
	}
	if s.opts.Listen == "" {
		return errors.New("proxy: listen address is required")
	}

	s.mu.Lock()
	switch {
	case s.stopped:
		s.mu.Unlock()
		return errServiceStopped
	case s.started:
		s.mu.Unlock()
		return errAlreadyStarted
	}
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("proxy: listen %s: %w", s.opts.Listen, err)
	}
	s.ln = ln
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.conns = make(map[net.Conn]struct{})
	s.sem = make(chan struct{}, s.maxConnections)
	s.started = true
	s.mu.Unlock()

	s.wg.Add(2)
	go s.acceptLoop()
	go s.watchContext()
	go func() {
		s.wg.Wait()
		close(s.done)
	}()
	return nil
}

// Stop closes the listener, cancels in-flight handlers, closes their client
// connections, and waits for the accept loop and handlers to exit. It is
// idempotent; the supplied context bounds the wait.
func (s *Service) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	started := s.started
	stopped := s.stopped
	cancel := s.cancel
	s.mu.Unlock()

	if !started && !stopped {
		return nil // never started
	}
	if cancel != nil {
		cancel()
	}
	s.shutdown()

	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Addr returns the bound listener address, or nil before Start. It exists so
// callers (and tests) can use 127.0.0.1:0 and discover the real port.
func (s *Service) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// watchContext turns a cancelled service context into the same shutdown path
// as Stop.
func (s *Service) watchContext() {
	defer s.wg.Done()
	<-s.ctx.Done()
	s.shutdown()
}

// shutdown stops new work and tears down current work. It is safe to call more
// than once.
func (s *Service) shutdown() {
	s.mu.Lock()
	s.stopped = true
	ln := s.ln
	active := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		active = append(active, c)
	}
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range active {
		_ = c.Close()
	}
}

// track registers an accepted connection unless shutdown already won. The
// per-connection goroutine is only started when track returns true.
func (s *Service) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Service) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Service) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}
