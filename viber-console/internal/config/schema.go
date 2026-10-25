package config

// FieldType is the JSON schema type of a config field.
type FieldType string

const (
	TypeString FieldType = "string"
	TypeInt    FieldType = "int"
	TypeBool   FieldType = "bool"
	TypeEnum   FieldType = "enum"
)

// Field describes one editable env var in the schema.
type Field struct {
	Key         string    `json:"key"`
	Group       string    `json:"group"` // "viberayd" | "viberoxy"
	Label       string    `json:"label"`
	Description string    `json:"description,omitempty"`
	Type        FieldType `json:"type"`
	Default     string    `json:"default,omitempty"`
	Min         *int      `json:"min,omitempty"`
	Max         *int      `json:"max,omitempty"`
	Enum        []string  `json:"enum,omitempty"`
	Required    bool      `json:"required,omitempty"`
	Secret      bool      `json:"secret,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
}

func intp(v int) *int { return &v }

// Fields is the full editable schema for both daemons.
// NOTE: keep in sync with the daemons' documented env vars.
var Fields = []Field{
	// ---- Viberayd ----
	{Key: "DAEMON_URLS_FILE", Group: "viberayd", Label: "URLs file", Description: "File with subscription URLs (one per line).", Type: TypeString, Default: "urls.txt", Required: true},
	{Key: "DAEMON_OUTPUT_FILE", Group: "viberayd", Label: "Output file", Description: "Working configs written here each cycle.", Type: TypeString, Default: "working.txt"},
	{Key: "DAEMON_STATE_FILE", Group: "viberayd", Label: "State file", Description: "Persisted state across restarts.", Type: TypeString, Default: "state.json"},
	{Key: "DAEMON_CYCLE_SLEEP", Group: "viberayd", Label: "Cycle sleep (s)", Description: "Seconds between test cycles.", Type: TypeInt, Default: "300", Min: intp(10)},
	{Key: "DAEMON_PARALLEL", Group: "viberayd", Label: "Parallel tests", Description: "Concurrent xray tests.", Type: TypeInt, Default: "10", Min: intp(1), Max: intp(20)},
	{Key: "DAEMON_TIMEOUT", Group: "viberayd", Label: "Test timeout (s)", Description: "Per-test timeout.", Type: TypeInt, Default: "10", Min: intp(1)},
	{Key: "DAEMON_DEPTH", Group: "viberayd", Label: "Test depth", Type: TypeEnum, Default: "standard", Enum: []string{"quick", "standard", "full", "comprehensive"}},
	{Key: "DAEMON_KEEP_SUCCESSFUL", Group: "viberayd", Label: "Keep successful", Description: "Re-test working configs.", Type: TypeBool, Default: "true"},
	{Key: "DAEMON_RETEST_INTERVAL", Group: "viberayd", Label: "Retest interval (s)", Description: "Seconds before re-testing a working config.", Type: TypeInt, Default: "1800", Min: intp(0)},
	{Key: "DAEMON_MAX_LATENCY_MS", Group: "viberayd", Label: "Max latency (ms)", Description: "Reject configs slower than this (0 = disabled).", Type: TypeInt, Default: "0", Min: intp(0)},
	{Key: "DAEMON_TCP_PING", Group: "viberayd", Label: "TCP ping gate", Description: "Fast TCP-connect prefilter before the xray test. Disable on networks that filter direct TCP to foreign hosts, or everything is marked unreachable.", Type: TypeBool, Default: "true"},
	{Key: "HTTP_ENABLED", Group: "viberayd", Label: "HTTP API enabled", Type: TypeBool, Default: "false"},
	{Key: "HTTP_PORT", Group: "viberayd", Label: "Sub port", Type: TypeInt, Default: "8080", Min: intp(0), Max: intp(65535)},
	{Key: "HTTP_SUB_PATH", Group: "viberayd", Label: "Sub path", Type: TypeString, Default: "/sub"},
	{Key: "HTTP_API_PORT", Group: "viberayd", Label: "API port", Type: TypeInt, Default: "8081", Min: intp(0), Max: intp(65535)},

	// ---- Viberoxy ----
	{Key: "SUBSCRIBER_URL", Group: "viberoxy", Label: "Subscription URL", Description: "URL to fetch proxy configs from.", Type: TypeString, Required: true, Secret: true, Placeholder: "https://example.com/sub"},
	{Key: "FETCH_INTERVAL", Group: "viberoxy", Label: "Fetch interval (s)", Description: "Seconds between fetch+test cycles.", Type: TypeInt, Default: "300", Min: intp(30)},
	{Key: "TEST_TIMEOUT", Group: "viberoxy", Label: "Test timeout (s)", Type: TypeInt, Default: "10", Min: intp(3)},
	{Key: "DOWNLOAD_SIZE", Group: "viberoxy", Label: "Download size (bytes)", Type: TypeInt, Default: "10000000", Min: intp(1000000)},
	{Key: "WAN_COUNT", Group: "viberoxy", Label: "WAN count", Description: "Max concurrent WANs.", Type: TypeInt, Default: "4", Min: intp(1), Max: intp(5)},
	{Key: "WAN_BASE_PORT", Group: "viberoxy", Label: "WAN base port", Type: TypeInt, Default: "10700", Min: intp(1), Max: intp(65535)},
	{Key: "TEST_BASE_PORT", Group: "viberoxy", Label: "Test base port", Type: TypeInt, Default: "10800", Min: intp(1), Max: intp(65535)},
	{Key: "PROXY_PORT", Group: "viberoxy", Label: "Proxy port", Type: TypeInt, Default: "1080", Min: intp(0), Max: intp(65535)},
	{Key: "SOCKS_PORT", Group: "viberoxy", Label: "SOCKS port", Description: "0 disables SOCKS5 front-end.", Type: TypeInt, Default: "0", Min: intp(0), Max: intp(65535)},
	{Key: "METRICS_PORT", Group: "viberoxy", Label: "Metrics port", Description: "0 disables /metrics + health.", Type: TypeInt, Default: "0", Min: intp(0), Max: intp(65535)},
	{Key: "MINIMUM_SPEED", Group: "viberoxy", Label: "Minimum speed (Mbps)", Type: TypeInt, Default: "5", Min: intp(0)},
	{Key: "MAX_TEST_PER_CYCLE", Group: "viberoxy", Label: "Max tests/cycle", Type: TypeInt, Default: "20", Min: intp(1)},
	{Key: "KEEPALIVE_INTERVAL", Group: "viberoxy", Label: "Keepalive interval (s)", Type: TypeInt, Default: "300", Min: intp(10)},
	{Key: "WAN_FAIL_THRESHOLD", Group: "viberoxy", Label: "WAN fail threshold", Type: TypeInt, Default: "2", Min: intp(1)},
	{Key: "STABILITY_PROBES", Group: "viberoxy", Label: "Stability probes", Type: TypeInt, Default: "0", Min: intp(0), Max: intp(5)},
	{Key: "ACCESS_LOG", Group: "viberoxy", Label: "Access log", Type: TypeBool, Default: "true"},
	{Key: "ALLOW_DEGRADED_BOOT", Group: "viberoxy", Label: "Allow degraded boot", Type: TypeBool, Default: "true"},
	{Key: "XRAY_MUX", Group: "viberoxy", Label: "Xray mux", Type: TypeBool, Default: "true"},
	{Key: "ROUTE_MODE", Group: "viberoxy", Label: "Route mode", Type: TypeEnum, Default: "all-proxy", Enum: []string{"all-proxy", "proxy-default", "direct-default"}},
	{Key: "DIRECT_DOMAINS", Group: "viberoxy", Label: "Direct domains", Description: "Comma-separated suffixes routed direct.", Type: TypeString, Placeholder: ".ir,.example.com"},
	{Key: "PROXY_DOMAINS", Group: "viberoxy", Label: "Proxy domains", Description: "Comma-separated suffixes routed via WAN.", Type: TypeString},
	{Key: "DIRECT_LIST_FILE", Group: "viberoxy", Label: "Direct list file", Type: TypeString},
	{Key: "PROXY_LIST_FILE", Group: "viberoxy", Label: "Proxy list file", Type: TypeString},
}

// FieldByKey returns the field for a key, or nil.
func FieldByKey(key string) *Field {
	for i := range Fields {
		if Fields[i].Key == key {
			return &Fields[i]
		}
	}
	return nil
}
