package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"dissident/internal/config"
	"dissident/internal/proxy"
	"dissident/internal/wan"
)

// proxyModule adapts proxy.Service to the app Module interface. The data-plane
// listener address is fixed for the process lifetime, so a proxy_listen change
// is rejected and the old generation stays in effect.
type proxyModule struct {
	svc *proxy.Service
	cfg *config.Manager
	log *slog.Logger
}

// Name implements Module.
func (m *proxyModule) Name() string { return "proxy" }

// Start implements Module.
func (m *proxyModule) Start(ctx context.Context) error { return m.svc.Start(ctx) }

// Stop implements Module.
func (m *proxyModule) Stop(ctx context.Context) error { return m.svc.Stop(ctx) }

// ValidateReload implements ReloadValidator.
func (m *proxyModule) ValidateReload(next *config.Snapshot) error {
	err := m.validate(next)
	if err != nil {
		m.log.Warn("proxy: config reload rejected; a restart is required", "err", err)
	}
	return err
}

// Reload implements Module. Only proxy_listen is owned here; other fields are
// accepted by this adapter and validated by their owning module.
func (m *proxyModule) Reload(next *config.Snapshot) error {
	if err := m.validate(next); err != nil {
		m.log.Warn("proxy: config reload rejected; a restart is required", "err", err)
		return err
	}
	m.log.Info("proxy config reload accepted", "generation", next.Generation)
	return nil
}

func (m *proxyModule) validate(next *config.Snapshot) error {
	if next == nil {
		return errors.New("proxy: nil config snapshot")
	}
	if m.cfg == nil {
		return nil
	}
	current := m.cfg.Current()
	if current == nil {
		return nil
	}
	want := next.Config.Server.ProxyListen
	have := current.Config.Server.ProxyListen
	if want != have {
		return fmt.Errorf("proxy: server.proxy_listen change from %q to %q requires restart", have, want)
	}
	return nil
}

// wanModule adapts wan.Manager to the app Module interface. The manager reads
// cycle/swap/ready values from the shared config manager on every use, so a
// reload is accepted for those. Qualification inputs are captured by the
// immutable qualify.Tester at process start: changing test.*, urls.*,
// egress_proxy, xray_bin, urls_file, or state_file would need a fresh tester
// (and a new state file), so such reloads are rejected instead of applying
// half of the new configuration.
type wanModule struct {
	mgr *wan.Manager
	cfg *config.Manager
	log *slog.Logger
}

// Name implements Module.
func (m *wanModule) Name() string { return "wan" }

// Start implements Module.
func (m *wanModule) Start(ctx context.Context) error { return m.mgr.Start(ctx) }

// Stop implements Module.
func (m *wanModule) Stop(ctx context.Context) error { return m.mgr.Stop(ctx) }

// ValidateReload implements ReloadValidator.
func (m *wanModule) ValidateReload(next *config.Snapshot) error {
	err := m.validate(next)
	if err != nil {
		m.log.Warn("wan: config reload rejected; a restart is required for the new qualification settings", "err", err)
	}
	return err
}

// Reload implements Module. It never tears down the running runtime; the
// manager reads live cycle/swap values itself.
func (m *wanModule) Reload(next *config.Snapshot) error {
	if err := m.validate(next); err != nil {
		m.log.Warn("wan: config reload rejected; a restart is required for the new qualification settings", "err", err)
		return err
	}
	m.log.Info("wan config reload accepted (cycle/swap/ready values are live)", "generation", next.Generation)
	return nil
}

func (m *wanModule) validate(next *config.Snapshot) error {
	if next == nil {
		return errors.New("wan: nil config snapshot")
	}
	if m.cfg == nil {
		return nil
	}
	current := m.cfg.Current()
	if current == nil {
		return nil
	}
	return validateWANReload(current.Config, next.Config)
}

// validateWANReload returns a clear error naming every field that would need a
// new qualification tester or a new runtime state file.
func validateWANReload(current, next config.Config) error {
	var blocked []string
	if current.Paths.XrayBin != next.Paths.XrayBin {
		blocked = append(blocked, "paths.xray_bin")
	}
	if current.Paths.URLsFile != next.Paths.URLsFile {
		blocked = append(blocked, "paths.urls_file")
	}
	if current.Paths.StateFile != next.Paths.StateFile {
		blocked = append(blocked, "paths.state_file")
	}
	if current.Test != next.Test {
		blocked = append(blocked, "test.* (including test.egress_proxy)")
	}
	if !slices.Equal(current.URLs.Probe, next.URLs.Probe) {
		blocked = append(blocked, "urls.probe")
	}
	if !slices.Equal(current.URLs.Verify, next.URLs.Verify) {
		blocked = append(blocked, "urls.verify")
	}
	if !slices.Equal(current.URLs.Content, next.URLs.Content) {
		blocked = append(blocked, "urls.content")
	}
	if current.URLs.Speed != next.URLs.Speed {
		blocked = append(blocked, "urls.speed")
	}
	if current.URLs.SpeedFallback != next.URLs.SpeedFallback {
		blocked = append(blocked, "urls.speed_fallback")
	}
	if current.URLs.ExitIP != next.URLs.ExitIP {
		blocked = append(blocked, "urls.exit_ip")
	}
	if len(blocked) == 0 {
		return nil
	}
	return fmt.Errorf("wan: %s change requires restart (the qualification tester and runtime state are fixed for this process)",
		strings.Join(blocked, ", "))
}
