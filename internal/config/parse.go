package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// rawEntry is one key/value pair from the INI file after section resolution.
type rawEntry struct {
	key   string
	value string
	line  int
}

// parseINI reads the small INI dialect used by dissident:
//
//   - comments start with '#' or ';' at the beginning of a trimmed line;
//   - sections are [name] and are prepended to keys as "name.key";
//   - top-level dotted keys such as server.panel_listen are accepted;
//   - key = value lines may have surrounding whitespace;
//   - values may be single- or double-quoted.
//
// Unknown keys, duplicate keys, and malformed lines are reported by apply.
func parseINI(data []byte) ([]rawEntry, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	lines := strings.Split(text, "\n")

	entries := make([]rawEntry, 0, len(lines))
	section := ""
	for i, rawLine := range lines {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: malformed section header %q", lineNo, line)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if !validName(name) {
				return nil, fmt.Errorf("line %d: invalid section name %q", lineNo, name)
			}
			section = strings.ToLower(name)
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", lineNo, line)
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		value := strings.TrimSpace(line[eq+1:])
		if !validName(key) {
			return nil, fmt.Errorf("line %d: invalid key %q", lineNo, key)
		}
		value = unquote(value)
		if section != "" {
			key = section + "." + key
		}
		entries = append(entries, rawEntry{key: key, value: value, line: lineNo})
	}
	return entries, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func applyEntries(c *Config, entries []rawEntry) error {
	seen := make(map[string]int, len(entries))
	for _, e := range entries {
		if prev, ok := seen[e.key]; ok {
			return fmt.Errorf("line %d: duplicate key %q (first set on line %d)", e.line, e.key, prev)
		}
		seen[e.key] = e.line
		if err := c.set(e.key, e.value); err != nil {
			return fmt.Errorf("line %d: %w", e.line, err)
		}
	}
	return nil
}

func (c *Config) set(key, value string) error {
	switch key {
	case "server.panel_listen":
		c.Server.PanelListen = value
	case "server.proxy_listen":
		c.Server.ProxyListen = value

	case "paths.urls_file":
		c.Paths.URLsFile = value
	case "paths.state_file":
		c.Paths.StateFile = value
	case "paths.log_dir":
		c.Paths.LogDir = value
	case "paths.xray_bin":
		c.Paths.XrayBin = value

	case "cycle.interval":
		return setDuration(&c.Cycle.Interval, value)
	case "cycle.active_liveness":
		return setDuration(&c.Cycle.ActiveLiveness, value)
	case "cycle.active_idle":
		return setDuration(&c.Cycle.ActiveIdle, value)
	case "cycle.ready_liveness":
		return setDuration(&c.Cycle.ReadyLiveness, value)
	case "cycle.probe_count":
		return setInt(&c.Cycle.ProbeCount, value)
	case "cycle.probe_window":
		return setDuration(&c.Cycle.ProbeWindow, value)
	case "cycle.probe_budget_per_min":
		return setInt(&c.Cycle.ProbeBudgetPerMin, value)
	case "cycle.ready_min":
		return setInt(&c.Cycle.ReadyMin, value)
	case "cycle.ready_target":
		return setInt(&c.Cycle.ReadyTarget, value)
	case "cycle.failed_retry":
		return setDuration(&c.Cycle.FailedRetry, value)

	case "swap.drain_timeout":
		return setDuration(&c.Swap.DrainTimeout, value)
	case "swap.promotion_timeout":
		return setDuration(&c.Swap.PromotionTimeout, value)

	case "test.cheap_workers":
		return setInt(&c.Test.CheapWorkers, value)
	case "test.real_workers":
		return setInt(&c.Test.RealWorkers, value)
	case "test.real_test_cap":
		return setInt(&c.Test.RealTestCap, value)
	case "test.tcp_timeout":
		return setDuration(&c.Test.TCPTimeout, value)
	case "test.tls_timeout":
		return setDuration(&c.Test.TLSTimeout, value)
	case "test.protocol_timeout":
		return setDuration(&c.Test.ProtocolTimeout, value)
	case "test.xray_timeout":
		return setDuration(&c.Test.XrayTimeout, value)
	case "test.http_timeout":
		return setDuration(&c.Test.HTTPTimeout, value)
	case "test.speed_size":
		return setInt64(&c.Test.SpeedSize, value)
	case "test.min_speed_mbps":
		return setFloat(&c.Test.MinSpeedMbps, value)
	case "test.max_ttfb_ms":
		return setInt(&c.Test.MaxTTFBMs, value)
	case "test.stability_passes":
		return setInt(&c.Test.StabilityPasses, value)
	case "test.content_quorum":
		return setInt(&c.Test.ContentQuorum, value)
	case "test.content_min_bytes":
		return setInt(&c.Test.ContentMinBytes, value)
	case "test.egress_proxy":
		c.Test.EgressProxy = strings.TrimSpace(value)

	case "urls.probe":
		c.URLs.Probe = splitList(value)
	case "urls.verify":
		c.URLs.Verify = splitList(value)
	case "urls.content":
		c.URLs.Content = splitList(value)
	case "urls.speed":
		c.URLs.Speed = value
	case "urls.speed_fallback":
		c.URLs.SpeedFallback = value
	case "urls.exit_ip":
		c.URLs.ExitIP = value

	case "log.level":
		c.Log.Level = strings.ToLower(value)
	case "log.access":
		return setBool(&c.Log.Access, value)
	case "log.retention_days":
		return setInt(&c.Log.RetentionDays, value)
	case "log.max_size_mb":
		return setInt(&c.Log.MaxSizeMB, value)

	default:
		return fmt.Errorf("unknown config key %q", key)
	}
	return nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func setDuration(dst *time.Duration, value string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("expected a duration such as 15s, got %q", value)
	}
	*dst = d
	return nil
}

func setInt(dst *int, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("expected an integer, got %q", value)
	}
	*dst = n
	return nil
}

func setInt64(dst *int64, value string) error {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("expected an integer, got %q", value)
	}
	*dst = n
	return nil
}

func setFloat(dst *float64, value string) error {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fmt.Errorf("expected a number, got %q", value)
	}
	*dst = f
	return nil
}

func setBool(dst *bool, value string) error {
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fmt.Errorf("expected true or false, got %q", value)
	}
	*dst = b
	return nil
}
