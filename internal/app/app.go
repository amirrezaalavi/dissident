package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/control"
	"dissident/internal/logging"
	"dissident/internal/proxy"
	"dissident/internal/qualify"
	"dissident/internal/wan"
)

// Options configures the application.
type Options struct {
	// Version is shown by the panel and log lines.
	Version string
	// ConfigPath is used by ReloadConfig. It may be empty when the process
	// only ever uses the supplied Config.
	ConfigPath string
	// Config is the initial, already-loaded configuration. New validates it.
	Config config.Config
	// Snapshot overrides the runtime view for the panel. It exists for tests
	// and embedding; when nil App.Snapshot returns the WAN manager's live view.
	// It must be safe for concurrent use.
	Snapshot func() contract.Snapshot
	// Stderr receives system log lines. Nil means os.Stderr.
	Stderr io.Writer
	// ControlListen overrides Config.Server.PanelListen. It exists for tests
	// and embedding; production wiring should leave it empty.
	ControlListen string
	// ProxyListen overrides Config.Server.ProxyListen. It exists for tests
	// and embedding; production wiring should leave it empty.
	ProxyListen string
}

// App owns the config manager, loggers, supervisor, control server, and the
// WAN/proxy runtime modules.
type App struct {
	version     string
	configPath  string
	manager     *config.Manager
	logs        *logging.Logging
	sup         *Supervisor
	control     *control.Server
	wan         *wan.Manager
	proxy       *proxy.Service
	provider    func() contract.Snapshot
	stopTimeout time.Duration

	startMu  sync.Mutex
	reloadMu sync.Mutex
	started  bool
}

// New validates the config and constructs (but does not start) the app.
func New(opts Options) (*App, error) {
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: invalid config: %w", err)
	}

	logs, err := logging.New(logging.Options{
		Dir:           cfg.Paths.LogDir,
		Level:         cfg.Log.Level,
		Access:        cfg.Log.Access,
		RetentionDays: cfg.Log.RetentionDays,
		MaxSizeMB:     cfg.Log.MaxSizeMB,
		Stderr:        opts.Stderr,
	})
	if err != nil {
		return nil, fmt.Errorf("app: initialize logging: %w", err)
	}

	manager, err := config.NewManager(cfg)
	if err != nil {
		_ = logs.Close()
		return nil, fmt.Errorf("app: initialize config manager: %w", err)
	}

	stopTimeout := cfg.Swap.DrainTimeout
	if stopTimeout <= 0 {
		stopTimeout = 30 * time.Second
	}

	// The qualification tester is immutable after construction. Reloads that
	// would need a different tester (test.*, urls.*, xray_bin, urls_file,
	// state_file, egress_proxy) are rejected by the WAN adapter so the old
	// generation keeps serving instead of running half-applied settings.
	store := catalog.New()
	tester := qualify.New(qualify.Options{
		Test:        cfg.Test,
		URLs:        cfg.URLs,
		XrayBin:     cfg.Paths.XrayBin,
		EgressProxy: cfg.Test.EgressProxy,
		Logger:      logs.System().With("component", "qualify"),
		TempDir:     filepath.Dir(cfg.Paths.StateFile),
	})
	wanMgr := wan.New(wan.Options{
		Catalog:   store,
		Tester:    tester,
		Config:    manager,
		StateFile: cfg.Paths.StateFile,
		Logger:    logs.System().With("component", "wan"),
	})

	proxyListen := cfg.Server.ProxyListen
	if opts.ProxyListen != "" {
		proxyListen = opts.ProxyListen
	}
	proxySvc := proxy.New(proxy.Options{
		Listen:   proxyListen,
		Provider: wanMgr,
		Access: func(rec contract.AccessRecord) {
			_ = logs.Access(rec)
		},
		Logger: logs.System().With("component", "proxy"),
	})

	controlListen := cfg.Server.PanelListen
	if opts.ControlListen != "" {
		controlListen = opts.ControlListen
	}
	controlSrv := control.New(control.Options{
		Listen:    controlListen,
		Data:      wanMgr,
		Config:    manager,
		AccessLog: filepath.Join(cfg.Paths.LogDir, "access.log"),
		SystemLog: filepath.Join(cfg.Paths.LogDir, "dissident.log"),
		Logger:    logs.System().With("component", "control"),
		Version:   opts.Version,
	})

	a := &App{
		version:     opts.Version,
		configPath:  opts.ConfigPath,
		manager:     manager,
		logs:        logs,
		control:     controlSrv,
		wan:         wanMgr,
		proxy:       proxySvc,
		provider:    opts.Snapshot,
		stopTimeout: stopTimeout,
	}

	a.sup = NewSupervisor(SupervisorOptions{
		Logger:      logs.System().With("component", "supervisor"),
		StopTimeout: stopTimeout,
	})
	// Start order: the panel first so it is live while the WAN manager
	// restores state, qualifies links, and promotes an active route; then the
	// proxy listener; then the WAN manager. Stop is reversed automatically.
	if err := a.sup.Add(controlSrv); err != nil {
		_ = logs.Close()
		return nil, fmt.Errorf("app: register control module: %w", err)
	}
	if err := a.sup.Add(&proxyModule{svc: proxySvc, cfg: manager, log: logs.System().With("component", "proxy")}); err != nil {
		_ = logs.Close()
		return nil, fmt.Errorf("app: register proxy module: %w", err)
	}
	if err := a.sup.Add(&wanModule{mgr: wanMgr, cfg: manager, log: logs.System().With("component", "wan")}); err != nil {
		_ = logs.Close()
		return nil, fmt.Errorf("app: register wan module: %w", err)
	}
	return a, nil
}

// Start starts the supervisor, which starts control, proxy, and WAN in that
// order. It succeeds with no active WAN; readiness reports "not ready" until
// one exists. If a module fails to start, the supervisor rolls back the
// modules already started and Start returns the error.
func (a *App) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if a.started {
		return errors.New("app: already started")
	}
	if err := a.sup.Start(ctx, a.manager.Current()); err != nil {
		return err
	}
	a.started = true
	current := a.manager.Current()
	stateFile := ""
	if current != nil {
		stateFile = current.Config.Paths.StateFile
	}
	a.logs.System().Info("dissident started",
		"version", a.version,
		"control_addr", a.ControlAddr(),
		"proxy_addr", a.ProxyAddr(),
		"state_file", stateFile,
		"config_generation", a.manager.Current().Generation,
	)
	return nil
}

// Stop stops modules in reverse order (WAN, proxy, control) and closes the log
// files. It is idempotent.
func (a *App) Stop(ctx context.Context) error {
	a.startMu.Lock()
	wasStarted := a.started
	a.started = false
	a.startMu.Unlock()

	var errs []error
	if wasStarted {
		if err := a.sup.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
		a.logs.System().Info("dissident stopped")
	}
	if err := a.logs.Close(); err != nil {
		errs = append(errs, fmt.Errorf("app: close logging: %w", err))
	}
	return errors.Join(errs...)
}

// Run starts the app, waits for ctx cancellation, then stops it with the
// configured drain timeout.
func (a *App) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("app: nil context")
	}
	if err := a.Start(ctx); err != nil {
		_ = a.Stop(context.Background())
		return err
	}
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), a.stopTimeout)
	defer cancel()
	return a.Stop(stopCtx)
}

// ReloadConfig reloads ConfigPath, applies it transactionally, and returns an
// error without changing runtime state if the file is invalid or a module
// rejects the new snapshot.
func (a *App) ReloadConfig() error {
	if a.configPath == "" {
		return errors.New("app: no config path configured")
	}
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	return a.ApplyConfig(cfg)
}

// ApplyConfig validates cfg, reloads every module transactionally, then
// publishes the new config generation. A rejected config leaves both the
// modules and the manager on the previous generation.
//
// Live fields: log.level and every cycle.* and swap.* value. Everything else
// either requires a restart (panel/proxy listeners, log_dir/log.access and
// rotation, paths.*) or would need a new immutable qualification tester
// (test.* including egress_proxy, urls.*, xray_bin, urls_file, state_file);
// those are rejected so the old generation keeps serving.
func (a *App) ApplyConfig(cfg config.Config) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	if err := cfg.Validate(); err != nil {
		return err
	}
	current := a.manager.Current()
	if current == nil {
		return errors.New("app: no current config snapshot")
	}
	if err := validateAppReload(current.Config, cfg); err != nil {
		return err
	}

	next := &config.Snapshot{Generation: current.Generation + 1, Config: cfg.Clone()}
	if err := a.sup.Reload(next); err != nil {
		return err
	}
	if err := a.manager.Apply(cfg); err != nil {
		// Defensive: Apply validates the same config, so this should not
		// happen. Restore modules to the previous snapshot if it does.
		if rerr := a.sup.Reload(current); rerr != nil {
			a.logs.System().Error("config rollback failed after manager rejection", "err", rerr)
		}
		return err
	}

	if err := a.logs.SetLevel(cfg.Log.Level); err != nil {
		a.logs.System().Warn("log level not applied", "err", err)
	}
	a.logs.System().Info("config applied", "generation", a.manager.Current().Generation)
	return nil
}

// validateAppReload rejects changes that the process cannot apply in place.
// Module-specific rules (panel/proxy listeners, tester inputs) live in the
// control server and the proxy/WAN adapters.
func validateAppReload(current, next config.Config) error {
	var blocked []string
	if current.Paths.LogDir != next.Paths.LogDir {
		blocked = append(blocked, "paths.log_dir")
	}
	if current.Log.Access != next.Log.Access {
		blocked = append(blocked, "log.access")
	}
	if current.Log.RetentionDays != next.Log.RetentionDays {
		blocked = append(blocked, "log.retention_days")
	}
	if current.Log.MaxSizeMB != next.Log.MaxSizeMB {
		blocked = append(blocked, "log.max_size_mb")
	}
	if len(blocked) > 0 {
		return fmt.Errorf("app: config change requires restart: %s", strings.Join(blocked, ", "))
	}
	return nil
}

// Snapshot returns the runtime view used by the control plane. A supplied
// provider wins; otherwise the live WAN manager view is returned. The zero
// Generation is filled from the config manager.
func (a *App) Snapshot() contract.Snapshot {
	var generation uint64
	if a.manager != nil {
		if current := a.manager.Current(); current != nil {
			generation = current.Generation
		}
	}
	var snap contract.Snapshot
	switch {
	case a.provider != nil:
		snap = a.provider()
	case a.wan != nil:
		snap = a.wan.Snapshot()
	default:
		snap = contract.Snapshot{Updated: time.Now().UTC()}
	}
	if snap.Generation == 0 {
		snap.Generation = generation
	}
	return snap
}

// Config exposes the config manager for read-only use and later modules.
func (a *App) Config() *config.Manager { return a.manager }

// ConfigPath returns the path passed in Options.
func (a *App) ConfigPath() string { return a.configPath }

// Logger returns the process system logger.
func (a *App) Logger() *slog.Logger { return a.logs.System() }

// ControlAddr returns the bound control-plane address after Start.
func (a *App) ControlAddr() string { return a.control.Addr() }

// ProxyAddr returns the bound data-plane address after Start, or "" before
// Start or when no proxy is wired.
func (a *App) ProxyAddr() string {
	if a.proxy == nil {
		return ""
	}
	addr := a.proxy.Addr()
	if addr == nil {
		return ""
	}
	return addr.String()
}
