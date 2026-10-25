package collector

import (
	"context"
	"strconv"
	"time"
)

// ViberoxyClient polls Viberoxy's observability and control endpoints.
// Metrics/health and the control API may be served on different listeners.
type ViberoxyClient struct {
	client     *Client
	metricsURL string
	apiURL     string
}

// NewViberoxyClient keeps the original single-listener behavior for callers
// that expose observability and control routes on one base URL.
func NewViberoxyClient(url string, timeout time.Duration) *ViberoxyClient {
	return NewViberoxyClientWithAPI(url, url, timeout)
}

// NewViberoxyClientWithAPI configures the observability and control listeners
// independently. Viberoxy serves these on separate ports by default.
func NewViberoxyClientWithAPI(metricsURL, apiURL string, timeout time.Duration) *ViberoxyClient {
	return &ViberoxyClient{
		client:     NewClient(timeout),
		metricsURL: metricsURL,
		apiURL:     apiURL,
	}
}

// WANSlot is the per-slot view extracted from viberoxy metrics.
type WANSlot struct {
	Index      int              `json:"index"`
	SpeedMbps  float64          `json:"speed_mbps"`
	Stability  int              `json:"stability"`
	Conns      int64            `json:"conns"`
	ProtoConns map[string]int64 `json:"proto_conns,omitempty"`
}

// ViberoxyProxy holds aggregate proxy counters + latency percentiles.
type ViberoxyProxy struct {
	ConnectionsTotal int64   `json:"connections_total"`
	BytesUp          int64   `json:"bytes_up"`
	BytesDown        int64   `json:"bytes_down"`
	LatencyP50S      float64 `json:"latency_p50_s"`
	LatencyP95S      float64 `json:"latency_p95_s"`
	LatencyP99S      float64 `json:"latency_p99_s"`
}

// ViberoxySnapshot is the full parsed view of one /metrics fetch.
type ViberoxySnapshot struct {
	WansActive   int           `json:"wans_active"`
	Slots        []WANSlot     `json:"slots"`
	Proxy        ViberoxyProxy `json:"proxy"`
	BuildVersion string        `json:"build_version,omitempty"`
}

// FetchMetrics pulls /metrics, parses Prometheus text, and builds the
// dashboard view.
func (v *ViberoxyClient) FetchMetrics(ctx context.Context) (ViberoxySnapshot, error) {
	body, err := v.client.Get(ctx, v.metricsURL+"/metrics")
	if err != nil {
		return ViberoxySnapshot{}, err
	}
	families, err := ParsePrometheusText(string(body))
	if err != nil {
		return ViberoxySnapshot{}, err
	}

	snap := ViberoxySnapshot{}

	if v, ok := FindGauge(families["viberoxy_wans_active"], nil); ok {
		snap.WansActive = int(v)
	}

	// Slots: union of indices seen in speed/stability gauges, plus per-slot
	// connection counts from the connections counter (wan label = slot index).
	slotIndexes := map[int]bool{}
	for _, s := range families["viberoxy_wan_speed_mbps"] {
		if idx, ok := parseIndex(s.Labels["index"]); ok {
			slotIndexes[idx] = true
		}
	}
	for _, s := range families["viberoxy_wan_stability"] {
		if idx, ok := parseIndex(s.Labels["index"]); ok {
			slotIndexes[idx] = true
		}
	}
	for _, s := range families["viberoxy_proxy_connections_total"] {
		if idx, ok := parseIndex(s.Labels["wan"]); ok {
			slotIndexes[idx] = true
		}
	}

	sorted := sortedIndexes(slotIndexes)
	for _, idx := range sorted {
		slot := WANSlot{Index: idx, ProtoConns: map[string]int64{}}

		if v, ok := FindGauge(families["viberoxy_wan_speed_mbps"], map[string]string{"index": strconv.Itoa(idx)}); ok {
			slot.SpeedMbps = v
		}
		if v, ok := FindGauge(families["viberoxy_wan_stability"], map[string]string{"index": strconv.Itoa(idx)}); ok {
			slot.Stability = int(v)
		}
		// Conns: sum over protocols for this wan.
		for _, s := range families["viberoxy_proxy_connections_total"] {
			if s.Labels["wan"] == strconv.Itoa(idx) {
				slot.Conns += int64(s.Value)
				slot.ProtoConns[s.Labels["proto"]] += int64(s.Value)
			}
		}
		if len(slot.ProtoConns) == 0 {
			slot.ProtoConns = nil
		}
		snap.Slots = append(snap.Slots, slot)
	}

	// Aggregate proxy counters. Note: viberoxy_proxy_connections_total is a
	// cumulative counter with labels wan+proto — sum across series.
	for _, s := range families["viberoxy_proxy_connections_total"] {
		snap.Proxy.ConnectionsTotal += int64(s.Value)
	}
	for _, s := range families["viberoxy_proxy_bytes_total"] {
		switch s.Labels["direction"] {
		case "up":
			snap.Proxy.BytesUp += int64(s.Value)
		case "down":
			snap.Proxy.BytesDown += int64(s.Value)
		}
	}

	// Latency percentiles from the histogram.
	if hist := families["viberoxy_proxy_latency_seconds"]; len(hist) > 0 && len(hist[0].Buckets) > 0 {
		snap.Proxy.LatencyP50S = Percentile(hist[0].Buckets, 50)
		snap.Proxy.LatencyP95S = Percentile(hist[0].Buckets, 95)
		snap.Proxy.LatencyP99S = Percentile(hist[0].Buckets, 99)
	}

	if v, ok := FindGauge(families["viberoxy_build_info"], nil); ok {
		_ = v
	}
	// build_info has version as a label; capture it from the series.
	for _, s := range families["viberoxy_build_info"] {
		if s.Labels["version"] != "" {
			snap.BuildVersion = s.Labels["version"]
			break
		}
	}

	return snap, nil
}

// FetchWANSlots fetches the per-slot WAN state from viberoxy's
// /api/viberoxy/wans endpoint and returns the raw JSON body along with the
// upstream HTTP status code. Errors indicate transport-level failures.
func (v *ViberoxyClient) FetchWANSlots(ctx context.Context) ([]byte, int, error) {
	return v.client.GetRaw(ctx, v.apiURL+"/api/viberoxy/wans")
}

// FetchCandidates fetches the candidate pool from viberoxy's
// /api/viberoxy/candidates endpoint and returns the raw JSON body along with
// the upstream HTTP status code. Errors indicate transport-level failures.
func (v *ViberoxyClient) FetchCandidates(ctx context.Context) ([]byte, int, error) {
	return v.client.GetRaw(ctx, v.apiURL+"/api/viberoxy/candidates")
}

// DropWAN sends a drop request to viberoxy's
// /api/viberoxy/wans/{index}/drop endpoint and returns the raw JSON response
// along with the upstream HTTP status code. Errors indicate transport-level
// failures; application-level errors are reflected in the status code.
func (v *ViberoxyClient) DropWAN(ctx context.Context, index int) ([]byte, int, error) {
	return v.client.Post(ctx, v.apiURL+"/api/viberoxy/wans/"+strconv.Itoa(index)+"/drop")
}

// TriggerCycle sends a manual cycle trigger to viberoxy's
// /api/viberoxy/cycle/trigger endpoint and returns the raw JSON response
// along with the upstream HTTP status code. Errors indicate transport-level
// failures.
func (v *ViberoxyClient) TriggerCycle(ctx context.Context) ([]byte, int, error) {
	return v.client.Post(ctx, v.apiURL+"/api/viberoxy/cycle/trigger")
}

// Healthz returns the raw healthz response body (or error).
func (v *ViberoxyClient) Healthz(ctx context.Context) (string, error) {
	body, err := v.client.Get(ctx, v.metricsURL+"/healthz")
	return string(body), err
}

// Readyz returns the raw readyz response body (or error).
func (v *ViberoxyClient) Readyz(ctx context.Context) (string, error) {
	body, err := v.client.Get(ctx, v.metricsURL+"/readyz")
	return string(body), err
}

func parseIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func sortedIndexes(m map[int]bool) []int {
	idx := make([]int, 0, len(m))
	for i := range m {
		idx = append(idx, i)
	}
	// insertion sort (small n)
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && idx[j] < idx[j-1]; j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
	return idx
}
