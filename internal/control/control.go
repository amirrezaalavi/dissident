// Package control implements the dissident server-rendered control panel.
// There is no SPA and no JSON control API: pages are rendered with
// html/template, forms POST and redirect, and metrics are served as
// Prometheus text.
package control

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

// Data is the runtime interface the panel needs. Implementations must be safe
// for concurrent use. When Data is nil the panel still serves Snapshot-only
// read-only pages.
type Data interface {
	Snapshot() contract.Snapshot
	Entries() []contract.Entry
	Cycling() bool
	LastCycle() time.Time
	NextCycle() time.Time
	LastError() string
	Replace(context.Context) error
	RunCycle(context.Context) error
	CancelCycle()
	RemoveReady(string) error
	Subscriptions() []string
	AddSubscription(string) error
	RemoveSubscription(string) error
}

// Options configures the control server.
type Options struct {
	// Listen is the panel listen address, for example 0.0.0.0:8090.
	Listen string
	// Data supplies the full runtime view and WAN/cycle/subscription actions.
	// Nil selects Snapshot-only read-only mode.
	Data Data
	// Snapshot returns the current runtime view. It is used only when Data is
	// nil. It must be safe to call concurrently; nil returns an empty snapshot.
	Snapshot func() contract.Snapshot
	// Config supplies the read-only settings view and readiness targets.
	Config *config.Manager
	// AccessLog is the access.log path shown on the logs page. Optional.
	AccessLog string
	// SystemLog is the dissident.log path shown on the logs page. Optional.
	SystemLog string
	// Logger receives lifecycle and handler-panic messages.
	Logger *slog.Logger
	// Version is displayed on the panel and in build_info metrics.
	Version string
	// ReadHeaderTimeout, ReadTimeout, WriteTimeout, and IdleTimeout bound HTTP
	// connections. Zero values select conservative defaults.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// Server is the control-plane Module. It is safe to construct before Start.
type Server struct {
	opts Options

	mu          sync.Mutex
	ln          net.Listener
	http        *http.Server
	started     bool
	stopped     bool
	addr        string
	panelListen string

	baseCtx context.Context
	cancel  context.CancelFunc
}

//go:embed templates/*.html
var templatesFS embed.FS

type pageTemplates struct {
	overview      *template.Template
	wan           *template.Template
	tests         *template.Template
	subscriptions *template.Template
	settings      *template.Template
	logs          *template.Template
}

var pages = mustParseTemplates()

func mustParseTemplates() pageTemplates {
	load := func(name string) *template.Template {
		return template.Must(template.New(name).ParseFS(
			templatesFS, "templates/base.html", "templates/"+name+".html"))
	}
	return pageTemplates{
		overview:      load("overview"),
		wan:           load("wan"),
		tests:         load("tests"),
		subscriptions: load("subscriptions"),
		settings:      load("settings"),
		logs:          load("logs"),
	}
}

// New returns a control server. Call Start to bind the listener.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Snapshot == nil {
		opts.Snapshot = func() contract.Snapshot { return contract.Snapshot{} }
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = 5 * time.Second
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 15 * time.Second
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = 30 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 60 * time.Second
	}
	panelListen := opts.Listen
	if opts.Config != nil {
		if cur := opts.Config.Current(); cur != nil {
			panelListen = cur.Config.Server.PanelListen
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		opts:        opts,
		panelListen: panelListen,
		baseCtx:     ctx,
		cancel:      cancel,
	}
}

// Name implements the app Module interface.
func (s *Server) Name() string { return "control" }

// Start binds the listener and serves in a background goroutine. It returns
// once the socket is bound, not when the server exits.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("control: already started")
	}
	if s.stopped {
		return errors.New("control: server was stopped and cannot be restarted")
	}
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return fmt.Errorf("control: listen on %s: %w", s.opts.Listen, err)
	}
	srv := &http.Server{
		Handler:           s.recoverer(s.routes()),
		ReadHeaderTimeout: s.opts.ReadHeaderTimeout,
		ReadTimeout:       s.opts.ReadTimeout,
		WriteTimeout:      s.opts.WriteTimeout,
		IdleTimeout:       s.opts.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.opts.Logger.Handler(), slog.LevelWarn),
	}
	s.ln = ln
	s.http = srv
	s.addr = ln.Addr().String()
	s.started = true
	s.opts.Logger.Info("control listening", "addr", s.addr, "version", s.opts.Version)

	go s.serve(srv)
	return nil
}

func (s *Server) serve(srv *http.Server) {
	defer func() {
		if rec := recover(); rec != nil {
			s.opts.Logger.Error("control serve panic", "panic", panicText(rec))
		}
	}()
	if err := srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.opts.Logger.Error("control server stopped", "err", sanitizeMessage(err.Error()))
	}
}

// Stop gracefully shuts the server down, bounded by ctx. It is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.stopped || !s.started {
		s.stopped = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		return nil
	}
	srv := s.http
	s.stopped = true
	s.started = false
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("control: shutdown: %w", err)
	}
	return nil
}

// Reload implements the app Module interface. A panel_listen change is
// rejected so the config manager keeps the previous generation; all other
// changes are accepted. Log paths stay fixed for the demo.
func (s *Server) Reload(snap *config.Snapshot) error {
	if snap == nil {
		return errors.New("control: nil config snapshot")
	}
	if snap.Config.Server.PanelListen != s.panelListen {
		return fmt.Errorf("control: panel_listen change from %q to %q requires restart",
			s.panelListen, snap.Config.Server.PanelListen)
	}
	s.opts.Logger.Info("control config reload accepted", "generation", snap.Generation)
	return nil
}

// Addr returns the bound address after Start, or "" before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Handler returns the HTTP handler tree, including panic recovery. It is
// exported so tests and future wiring can mount it without binding a socket.
func (s *Server) Handler() http.Handler {
	return s.recoverer(s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleOverview)
	mux.HandleFunc("GET /wan", s.handleWAN)
	mux.HandleFunc("POST /wan/replace", s.handleWANReplace)
	mux.HandleFunc("POST /wan/remove", s.handleWANRemove)
	mux.HandleFunc("GET /tests", s.handleTests)
	mux.HandleFunc("POST /cycle/run", s.handleCycleRun)
	mux.HandleFunc("POST /cycle/cancel", s.handleCycleCancel)
	mux.HandleFunc("GET /subscriptions", s.handleSubscriptions)
	mux.HandleFunc("POST /subscriptions/add", s.handleSubscriptionAdd)
	mux.HandleFunc("POST /subscriptions/remove", s.handleSubscriptionRemove)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /logs", s.handleLogs)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	return mux
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.opts.Logger.Error("control handler panic", "panic", panicText(rec), "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) snapshot() contract.Snapshot {
	var snap contract.Snapshot
	if s.opts.Data != nil {
		snap = s.opts.Data.Snapshot()
	} else if s.opts.Snapshot != nil {
		snap = s.opts.Snapshot()
	}
	if snap.Generation == 0 && s.opts.Config != nil {
		if cur := s.opts.Config.Current(); cur != nil {
			snap.Generation = cur.Generation
		}
	}
	return snap
}

func (s *Server) entries() []contract.Entry {
	if s.opts.Data == nil {
		return nil
	}
	return s.opts.Data.Entries()
}

func (s *Server) cycling() bool {
	return s.opts.Data != nil && s.opts.Data.Cycling()
}

func (s *Server) lastCycle() time.Time {
	if s.opts.Data == nil {
		return time.Time{}
	}
	return s.opts.Data.LastCycle()
}

func (s *Server) nextCycle() time.Time {
	if s.opts.Data == nil {
		return time.Time{}
	}
	return s.opts.Data.NextCycle()
}

func (s *Server) lastError() string {
	if s.opts.Data == nil {
		return ""
	}
	return s.opts.Data.LastError()
}

func (s *Server) subscriptions() []string {
	if s.opts.Data == nil {
		return nil
	}
	return s.opts.Data.Subscriptions()
}

func (s *Server) readyTarget() int {
	if s.opts.Config != nil {
		if cur := s.opts.Config.Current(); cur != nil {
			return cur.Config.Cycle.ReadyTarget
		}
	}
	return 5
}

func (s *Server) readyMin() int {
	if s.opts.Config != nil {
		if cur := s.opts.Config.Current(); cur != nil {
			return cur.Config.Cycle.ReadyMin
		}
	}
	return 3
}

type baseData struct {
	Title         string
	Nav           string
	Version       string
	Flash         string
	FlashError    bool
	Notice        string
	Refresh       bool
	DataAvailable bool
}

func (s *Server) newBase(r *http.Request, title, nav string) baseData {
	flash := sanitizeMessage(r.URL.Query().Get("msg"))
	return baseData{
		Title:         title,
		Nav:           nav,
		Version:       s.opts.Version,
		Flash:         flash,
		FlashError:    flash != "" && r.URL.Query().Get("err") != "",
		DataAvailable: s.opts.Data != nil,
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "base", data); err != nil {
		s.opts.Logger.Error("control page render failed", "path", r.URL.Path, "err", sanitizeMessage(err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string, isErr bool) {
	msg = sanitizeMessage(msg)
	if msg == "" {
		msg = "done"
	}
	q := url.Values{}
	q.Set("msg", msg)
	if isErr {
		q.Set("err", "1")
	}
	http.Redirect(w, r, path+"?"+q.Encode(), http.StatusSeeOther)
}

func (s *Server) rejectCrossSite(w http.ResponseWriter, r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		http.Error(w, "cross-site form submission rejected", http.StatusForbidden)
		return true
	}
	return false
}

func panicText(rec any) string {
	text := sanitizeMessage(fmt.Sprint(rec))
	if text == "" {
		return "panic"
	}
	return text
}

func (s *Server) requireData(w http.ResponseWriter, r *http.Request, back string) bool {
	if s.opts.Data != nil {
		return true
	}
	s.redirectFlash(w, r, back, "runtime data provider is not configured", true)
	return false
}

func (s *Server) resolveReadyHash(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", errors.New("missing entry hash")
	}
	seen := make(map[string]struct{})
	var matches []string
	for _, r := range s.snapshot().Ready {
		h := strings.TrimSpace(r.Link.Hash)
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		if strings.HasPrefix(strings.ToLower(h), strings.ToLower(prefix)) {
			seen[h] = struct{}{}
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 0:
		return "", errors.New("no ready entry matches that hash prefix")
	case 1:
		return matches[0], nil
	default:
		return "", errors.New("hash prefix matches more than one ready entry")
	}
}

func clampLines(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return 200
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 200
	}
	if n > 1000 {
		return 1000
	}
	return n
}
