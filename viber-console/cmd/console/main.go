// Command console runs the viber-console backend: a read-only aggregation
// API over Viberayd (aggregator) and Viberoxy (proxy) for the future WebUI.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yolka-wiz/viber-console/internal/collector"
	"github.com/yolka-wiz/viber-console/internal/config"
	"github.com/yolka-wiz/viber-console/internal/dashboard"
	"github.com/yolka-wiz/viber-console/internal/supervisor"
)

type settings struct {
	listen       string
	pollInterval time.Duration
	vdAPI        string
	vdSub        string
	vxMetrics    string
	vxAPI        string
	logLevel     string
	configDir    string
	viberaydBin  string
	viberoxyBin  string
	token        string
}

func loadConfig() settings {
	cfg := settings{
		listen:       env("CONSOLE_LISTEN", ":8090"),
		pollInterval: envDuration("CONSOLE_POLL_INTERVAL", 10*time.Second),
		vdAPI:        env("VIBERAYD_API_URL", "http://127.0.0.1:8081"),
		vdSub:        env("VIBERAYD_SUB_URL", "http://127.0.0.1:8080"),
		vxMetrics:    env("VIBEROXY_METRICS_URL", "http://127.0.0.1:9090"),
		vxAPI:        env("VIBEROXY_API_URL", "http://127.0.0.1:1980"),
		logLevel:     env("CONSOLE_LOG_LEVEL", "info"),
		configDir:    env("VIBER_CONFIG_DIR", "/etc/viber"),
		viberaydBin:  env("VIBERAYD_BIN", "viberayd"),
		viberoxyBin:  env("VIBEROXY_BIN", "viberoxy"),
		token:        os.Getenv("CONSOLE_TOKEN"),
	}
	return cfg
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		slog.Warn("invalid duration, using default", "key", key, "value", v, "default", def)
	}
	return def
}

func main() {
	cfg := loadConfig()

	level := slog.LevelInfo
	if cfg.logLevel == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	vd := collector.NewViberaydClient(cfg.vdAPI, cfg.vdSub, 5*time.Second)
	vx := collector.NewViberoxyClientWithAPI(cfg.vxMetrics, cfg.vxAPI, 5*time.Second)

	store := dashboard.NewStore(cfg.pollInterval, vd, vx)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go store.Run(ctx)

	// Supervised services: console owns the daemons' lifecycle.
	cfgStore := config.NewStore(cfg.configDir)
	services := []*supervisor.Service{
		supervisor.NewService("viberayd", cfg.viberaydBin, cfgStore.FileName("viberayd")),
		supervisor.NewService("viberoxy", cfg.viberoxyBin, cfgStore.FileName("viberoxy")),
	}
	for _, s := range services {
		if err := s.Start(ctx); err != nil {
			slog.Warn("service failed to start (continuing; may be launched separately)", "service", s.Name, "error", err)
		}
	}
	defer func() {
		for _, s := range services {
			s.Stop()
		}
	}()

	handler := dashboard.NewHandler(store)
	control := dashboard.NewControlHandler(store, cfgStore, services, cfg.vdAPI, cfg.token)
	mux := handler.Routes()
	control.Routes(mux)
	mux.Handle("/static/", dashboard.StaticHandler())
	mux.Handle("/", dashboard.StaticHandler())
	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("viber-console listening", "addr", cfg.listen, "poll_interval", cfg.pollInterval.String())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		slog.Error("server error", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown", "error", err)
		}
	}

	slog.Info("bye")
}
