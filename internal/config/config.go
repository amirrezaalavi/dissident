// Package config defines the single dissident configuration file, its
// defaults, validation rules, and the concurrency-safe runtime snapshot.
package config

import "time"

// Config is one complete, validated dissident configuration.
type Config struct {
	Server ServerConfig
	Paths  PathsConfig
	Cycle  CycleConfig
	Swap   SwapConfig
	Test   TestConfig
	URLs   URLsConfig
	Log    LogConfig
}

// ServerConfig holds the control-panel and data-plane listen addresses.
type ServerConfig struct {
	PanelListen string
	ProxyListen string
}

// PathsConfig holds filesystem locations used by dissident.
type PathsConfig struct {
	URLsFile  string
	StateFile string
	LogDir    string
	XrayBin   string
}

// CycleConfig controls catalog/qualification cadence and readiness targets.
type CycleConfig struct {
	Interval          time.Duration
	ActiveLiveness    time.Duration
	ActiveIdle        time.Duration
	ReadyLiveness     time.Duration
	ProbeCount        int
	ProbeWindow       time.Duration
	ProbeBudgetPerMin int
	ReadyMin          int
	ReadyTarget       int
}

// SwapConfig bounds active-WAN drain and standby promotion.
type SwapConfig struct {
	DrainTimeout     time.Duration
	PromotionTimeout time.Duration
}

// TestConfig bounds the qualification pipeline.
type TestConfig struct {
	CheapWorkers    int
	RealWorkers     int
	RealTestCap     int
	TCPTimeout      time.Duration
	TLSTimeout      time.Duration
	ProtocolTimeout time.Duration
	XrayTimeout     time.Duration
	HTTPTimeout     time.Duration
	SpeedSize       int64
	MinSpeedMbps    float64
	MaxTTFBMs       int
	StabilityPasses int
	ContentQuorum   int
	ContentMinBytes int
	// EgressProxy is an optional HTTP(S) CONNECT proxy used for the demo
	// host's outbound network. Empty means dial directly. It is not a
	// routing feature and never becomes a direct-egress fallback.
	EgressProxy string
}

// URLsConfig holds probe, verification, content, speed, and exit-IP targets.
type URLsConfig struct {
	Probe         []string
	Verify        []string
	Content       []string
	Speed         string
	SpeedFallback string
	ExitIP        string
}

// LogConfig controls logging and log-file rotation.
type LogConfig struct {
	Level         string
	Access        bool
	RetentionDays int
	MaxSizeMB     int
}

// Defaults returns the configuration used when a key is absent. The values
// mirror the documented demo defaults in docs/DISSIDENT-IMPLEMENTATION.md.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			PanelListen: "0.0.0.0:8090",
			ProxyListen: "0.0.0.0:10909",
		},
		Paths: PathsConfig{
			URLsFile:  "./configs/urls.txt",
			StateFile: "./var/state.json",
			LogDir:    "./var/log",
			XrayBin:   "/usr/local/bin/xray",
		},
		Cycle: CycleConfig{
			Interval:          1800 * time.Second,
			ActiveLiveness:    15 * time.Second,
			ActiveIdle:        60 * time.Second,
			ReadyLiveness:     100 * time.Second,
			ProbeCount:        3,
			ProbeWindow:       8 * time.Second,
			ProbeBudgetPerMin: 60,
			ReadyMin:          3,
			ReadyTarget:       5,
		},
		Swap: SwapConfig{
			DrainTimeout:     30 * time.Second,
			PromotionTimeout: 15 * time.Second,
		},
		Test: TestConfig{
			CheapWorkers:    8,
			RealWorkers:     1,
			RealTestCap:     0,
			TCPTimeout:      3 * time.Second,
			TLSTimeout:      3 * time.Second,
			ProtocolTimeout: 3 * time.Second,
			XrayTimeout:     10 * time.Second,
			HTTPTimeout:     5 * time.Second,
			SpeedSize:       4000000,
			MinSpeedMbps:    5,
			MaxTTFBMs:       2500,
			StabilityPasses: 2,
			ContentQuorum:   3,
			ContentMinBytes: 4096,
		},
		URLs: URLsConfig{
			Probe: []string{
				"https://www.gstatic.com/generate_204",
				"https://www.cloudflare.com/cdn-cgi/trace",
			},
			Verify: []string{
				"https://www.google.com/generate_204",
				"https://1.1.1.1/cdn-cgi/trace",
			},
			Content: []string{
				"https://youtube.com",
				"https://www.cloudflare.com",
				"https://telegram.org/",
				"https://www.instagram.com/",
			},
			Speed:         "https://proof.ovh.net/files/10Mb.dat",
			SpeedFallback: "https://speed.cloudflare.com/__down?bytes=",
			ExitIP:        "https://api.ipify.org/",
		},
		Log: LogConfig{
			Level:         "info",
			Access:        true,
			RetentionDays: 30,
			MaxSizeMB:     100,
		},
	}
}

// Clone returns a deep copy of c. Slices are copied so callers cannot mutate
// the configuration held by a Manager snapshot.
func (c Config) Clone() Config {
	out := c
	out.URLs.Probe = cloneStrings(c.URLs.Probe)
	out.URLs.Verify = cloneStrings(c.URLs.Verify)
	out.URLs.Content = cloneStrings(c.URLs.Content)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
