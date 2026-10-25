package collector

import (
	"math"
	"testing"
)

const testMetrics = `# HELP viberoxy_wans_active Number of active WAN slots.
# TYPE viberoxy_wans_active gauge
viberoxy_wans_active 2
# HELP viberoxy_wan_speed_mbps Last measured speed in Mbps per WAN slot.
# TYPE viberoxy_wan_speed_mbps gauge
viberoxy_wan_speed_mbps{index="0"} 42.5
viberoxy_wan_speed_mbps{index="1"} 18.2
# HELP viberoxy_wan_stability Stability score per WAN slot.
# TYPE viberoxy_wan_stability gauge
viberoxy_wan_stability{index="0"} 0
viberoxy_wan_stability{index="1"} 2
# HELP viberoxy_proxy_connections_total Total CONNECT attempts handled by the proxy.
# TYPE viberoxy_proxy_connections_total counter
viberoxy_proxy_connections_total{wan="0",proto="connect"} 10
viberoxy_proxy_connections_total{wan="0",proto="socks5"} 5
viberoxy_proxy_connections_total{wan="1",proto="connect"} 7
# HELP viberoxy_proxy_bytes_total Bytes relayed through the proxy.
# TYPE viberoxy_proxy_bytes_total counter
viberoxy_proxy_bytes_total{wan="0",direction="up"} 1000
viberoxy_proxy_bytes_total{wan="0",direction="down"} 5000
viberoxy_proxy_bytes_total{wan="1",direction="up"} 2000
viberoxy_proxy_bytes_total{wan="1",direction="down"} 9000
# HELP viberoxy_proxy_latency_seconds Tunnel latency from CONNECT to close, in seconds.
# TYPE viberoxy_proxy_latency_seconds histogram
viberoxy_proxy_latency_seconds_bucket{le="0.05"} 8
viberoxy_proxy_latency_seconds_bucket{le="0.1"} 12
viberoxy_proxy_latency_seconds_bucket{le="0.25"} 18
viberoxy_proxy_latency_seconds_bucket{le="0.5"} 20
viberoxy_proxy_latency_seconds_bucket{le="1"} 20
viberoxy_proxy_latency_seconds_bucket{le="2"} 20
viberoxy_proxy_latency_seconds_bucket{le="4"} 20
viberoxy_proxy_latency_seconds_bucket{le="8"} 20
viberoxy_proxy_latency_seconds_bucket{le="16"} 20
viberoxy_proxy_latency_seconds_bucket{le="+Inf"} 20
viberoxy_proxy_latency_seconds_sum 2.5
viberoxy_proxy_latency_seconds_count 20
# HELP viberoxy_build_info Build information.
# TYPE viberoxy_build_info gauge
viberoxy_build_info{version="v0.1.0"} 1
`

func TestParsePrometheusText_Basic(t *testing.T) {
	families, err := ParsePrometheusText(testMetrics)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if v, ok := FindGauge(families["viberoxy_wans_active"], nil); !ok || v != 2 {
		t.Errorf("wans_active = %v (ok=%v), want 2", v, ok)
	}

	if v, ok := FindGauge(families["viberoxy_wan_speed_mbps"], map[string]string{"index": "1"}); !ok || v != 18.2 {
		t.Errorf("speed index=1 = %v (ok=%v), want 18.2", v, ok)
	}

	if v, ok := FindGauge(families["viberoxy_wan_stability"], map[string]string{"index": "1"}); !ok || v != 2 {
		t.Errorf("stability index=1 = %v (ok=%v), want 2", v, ok)
	}
}

func TestParsePrometheusText_Histogram(t *testing.T) {
	families, err := ParsePrometheusText(testMetrics)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	hist := families["viberoxy_proxy_latency_seconds"]
	if len(hist) != 1 {
		t.Fatalf("expected 1 histogram sample, got %d", len(hist))
	}
	h := hist[0]
	if len(h.Buckets) != 9 {
		t.Fatalf("expected 9 buckets (no +Inf), got %d", len(h.Buckets))
	}
	if h.Buckets[0].Le != 0.05 || h.Buckets[0].Count != 8 {
		t.Errorf("bucket0 = %+v, want le=0.05 count=8", h.Buckets[0])
	}
	if h.Buckets[8].Le != 16 || h.Buckets[8].Count != 20 {
		t.Errorf("bucket8 = %+v, want le=16 count=20", h.Buckets[8])
	}
	if h.Count != 20 || math.Abs(h.Sum-2.5) > 1e-9 {
		t.Errorf("count=%d sum=%v, want 20 / 2.5", h.Count, h.Sum)
	}
}

func TestPercentile(t *testing.T) {
	buckets := []HistogramBucket{
		{Le: 0.05, Count: 8},
		{Le: 0.1, Count: 12},
		{Le: 0.25, Count: 18},
		{Le: 0.5, Count: 20},
		{Le: 1, Count: 20},
	}
	// p50: target = 20*0.5 = 10 -> in bucket 0.05..0.1 (counts 8..12)
	p50 := Percentile(buckets, 50)
	if p50 < 0.05 || p50 > 0.1 {
		t.Errorf("p50 = %v, want between 0.05 and 0.1", p50)
	}
	// p95: target = 19 -> in bucket 0.25..0.5 (counts 18..20)
	p95 := Percentile(buckets, 95)
	if p95 < 0.25 || p95 > 0.5 {
		t.Errorf("p95 = %v, want between 0.25 and 0.5", p95)
	}
	// p100: target = 20 -> exactly last bucket edge
	if p100 := Percentile(buckets, 100); p100 != 0.5 {
		t.Errorf("p100 = %v, want 0.5", p100)
	}
	// empty
	if v := Percentile(nil, 50); v != 0 {
		t.Errorf("empty percentile = %v, want 0", v)
	}
}

func TestFindGauge_NoMatch(t *testing.T) {
	families, _ := ParsePrometheusText(testMetrics)
	if v, ok := FindGauge(families["viberoxy_wan_speed_mbps"], map[string]string{"index": "9"}); ok {
		t.Errorf("index=9 = %v, want no match", v)
	}
	if _, ok := FindGauge(families["nonexistent"], nil); ok {
		t.Error("nonexistent family matched")
	}
}

func TestParsePrometheusText_Malformed(t *testing.T) {
	if _, err := ParsePrometheusText("not-a-metric-line"); err == nil {
		t.Error("expected error for malformed line")
	}
	if _, err := ParsePrometheusText("name{unbalanced 1"); err == nil {
		t.Error("expected error for unbalanced braces")
	}
}
