package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Validate checks every field and cross-field invariant. It returns an error
// listing all problems so a rejected configuration can be fixed in one pass.
func (c Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if err := validateListen("server.panel_listen", c.Server.PanelListen); err != nil {
		add("%v", err)
	}
	if err := validateListen("server.proxy_listen", c.Server.ProxyListen); err != nil {
		add("%v", err)
	}
	if strings.TrimSpace(c.Server.PanelListen) == strings.TrimSpace(c.Server.ProxyListen) {
		add("server.panel_listen and server.proxy_listen must differ (both are %q)", c.Server.PanelListen)
	}

	for _, p := range []struct {
		key   string
		value string
	}{
		{"paths.urls_file", c.Paths.URLsFile},
		{"paths.state_file", c.Paths.StateFile},
		{"paths.log_dir", c.Paths.LogDir},
		{"paths.xray_bin", c.Paths.XrayBin},
	} {
		if strings.TrimSpace(p.value) == "" {
			add("%s must not be empty", p.key)
		}
	}

	if c.Cycle.Interval <= 0 {
		add("cycle.interval must be greater than zero")
	}
	if c.Cycle.ActiveLiveness <= 0 {
		add("cycle.active_liveness must be greater than zero")
	}
	if c.Cycle.ActiveIdle <= 0 {
		add("cycle.active_idle must be greater than zero")
	}
	if c.Cycle.ReadyLiveness <= 0 {
		add("cycle.ready_liveness must be greater than zero")
	}
	if c.Cycle.ProbeCount < 1 {
		add("cycle.probe_count must be at least 1")
	}
	if c.Cycle.ProbeWindow <= 0 {
		add("cycle.probe_window must be greater than zero")
	}
	if c.Cycle.ProbeBudgetPerMin < 1 {
		add("cycle.probe_budget_per_min must be at least 1")
	}
	if c.Cycle.ReadyMin < 1 {
		add("cycle.ready_min must be at least 1")
	}
	if c.Cycle.ReadyTarget < 1 {
		add("cycle.ready_target must be at least 1")
	}
	if c.Cycle.ReadyMin > c.Cycle.ReadyTarget {
		add("cycle.ready_min (%d) must not exceed cycle.ready_target (%d)", c.Cycle.ReadyMin, c.Cycle.ReadyTarget)
	}

	if c.Swap.DrainTimeout <= 0 {
		add("swap.drain_timeout must be greater than zero")
	}
	if c.Swap.PromotionTimeout <= 0 {
		add("swap.promotion_timeout must be greater than zero")
	}

	if c.Test.CheapWorkers < 1 {
		add("test.cheap_workers must be at least 1")
	}
	if c.Test.RealWorkers < 1 {
		add("test.real_workers must be at least 1")
	}
	if c.Test.RealTestCap < 0 {
		add("test.real_test_cap must not be negative")
	}
	for _, d := range []struct {
		key   string
		value int64
	}{
		{"test.tcp_timeout", int64(c.Test.TCPTimeout)},
		{"test.tls_timeout", int64(c.Test.TLSTimeout)},
		{"test.protocol_timeout", int64(c.Test.ProtocolTimeout)},
		{"test.xray_timeout", int64(c.Test.XrayTimeout)},
		{"test.http_timeout", int64(c.Test.HTTPTimeout)},
	} {
		if d.value <= 0 {
			add("%s must be greater than zero", d.key)
		}
	}
	if c.Test.SpeedSize <= 0 {
		add("test.speed_size must be greater than zero")
	}
	if c.Test.MinSpeedMbps < 0 {
		add("test.min_speed_mbps must not be negative")
	}
	if c.Test.MaxTTFBMs <= 0 {
		add("test.max_ttfb_ms must be greater than zero")
	}
	if c.Test.StabilityPasses < 1 {
		add("test.stability_passes must be at least 1")
	}
	if c.Test.ContentQuorum < 1 {
		add("test.content_quorum must be at least 1")
	}
	if c.Test.ContentMinBytes < 1 {
		add("test.content_min_bytes must be at least 1")
	}
	if c.Test.EgressProxy != "" {
		if err := validateHTTPURL(c.Test.EgressProxy); err != nil {
			add("test.egress_proxy: %v", err)
		}
	}

	if err := validateURLList("urls.probe", c.URLs.Probe); err != nil {
		add("%v", err)
	}
	if err := validateURLList("urls.verify", c.URLs.Verify); err != nil {
		add("%v", err)
	}
	if err := validateURLList("urls.content", c.URLs.Content); err != nil {
		add("%v", err)
	}
	if err := validateOptionalURL("urls.speed", c.URLs.Speed); err != nil {
		add("%v", err)
	}
	if err := validateOptionalURL("urls.speed_fallback", c.URLs.SpeedFallback); err != nil {
		add("%v", err)
	}
	if err := validateOptionalURL("urls.exit_ip", c.URLs.ExitIP); err != nil {
		add("%v", err)
	}
	if c.Test.ContentQuorum > len(c.URLs.Content) {
		add("test.content_quorum (%d) must not exceed the number of urls.content entries (%d)",
			c.Test.ContentQuorum, len(c.URLs.Content))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be one of debug, info, warn, error (got %q)", c.Log.Level)
	}
	if c.Log.RetentionDays < 1 {
		add("log.retention_days must be at least 1")
	}
	if c.Log.MaxSizeMB < 1 {
		add("log.max_size_mb must be at least 1")
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid config:\n  - %s", strings.Join(problems, "\n  - "))
}

func validateListen(key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s must not be empty", key)
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s: %q must be host:port: %v", key, value, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("%s: %q has an invalid port (want 0-65535)", key, value)
	}
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("%s: %q has an invalid host", key, value)
		}
	}
	return nil
}

func validateURLList(key string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s must contain at least one URL", key)
	}
	for i, raw := range values {
		if err := validateHTTPURL(raw); err != nil {
			return fmt.Errorf("%s[%d]: %v", key, i, err)
		}
	}
	return nil
}

func validateOptionalURL(key, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	return nil
}

func validateHTTPURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid URL %q: missing host", raw)
	}
	return nil
}
