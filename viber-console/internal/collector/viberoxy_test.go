package collector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeViberoxyControlAPI returns an httptest.Server that serves the new
// viberoxy control endpoints: /api/viberoxy/wans, /api/viberoxy/candidates,
// /api/viberoxy/wans/{i}/drop, /api/viberoxy/cycle/trigger.
func fakeViberoxyControlAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/viberoxy/wans", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"index":0,"state":"active","speed_mbps":42.5,"conns":7,"exit_ip":"203.0.113.1"},{"index":1,"state":"empty","speed_mbps":0,"conns":0}]`))
	})

	mux.HandleFunc("/api/viberoxy/candidates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"cfg1","server":"1.2.3.4","port":443,"protocol":"vless","speed_mbps":55.2},{"name":"cfg2","server":"5.6.7.8","port":8443,"protocol":"ss","speed_mbps":30.1}]`))
	})

	mux.HandleFunc("/api/viberoxy/wans/0/drop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"replaced","wan":{"index":0,"state":"active","speed_mbps":73.5}}`))
	})

	mux.HandleFunc("/api/viberoxy/wans/99/drop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"status":"error","message":"invalid WAN index"}`))
	})

	mux.HandleFunc("/api/viberoxy/cycle/trigger", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted","queued":true}`))
	})

	return httptest.NewServer(mux)
}

func TestViberoxyUsesSeparateMetricsAndAPIURLs(t *testing.T) {
	metricsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("viberoxy_wans_active 1\n"))
	}))
	defer metricsSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/viberoxy/wans" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"index":0,"state":"active"}]`))
	}))
	defer apiSrv.Close()

	client := NewViberoxyClientWithAPI(metricsSrv.URL, apiSrv.URL, 3*time.Second)
	metrics, err := client.FetchMetrics(context.Background())
	if err != nil {
		t.Fatalf("FetchMetrics: %v", err)
	}
	if metrics.WansActive != 1 {
		t.Fatalf("WansActive = %d, want 1", metrics.WansActive)
	}

	body, status, err := client.FetchWANSlots(context.Background())
	if err != nil {
		t.Fatalf("FetchWANSlots: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(body) != `[{"index":0,"state":"active"}]` {
		t.Fatalf("body = %s", body)
	}
}

func TestViberoxyFetchWANSlots(t *testing.T) {
	srv := fakeViberoxyControlAPI(t)
	defer srv.Close()

	client := NewViberoxyClient(srv.URL, 3*time.Second)
	body, status, err := client.FetchWANSlots(context.Background())
	if err != nil {
		t.Fatalf("FetchWANSlots: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	var slots []map[string]interface{}
	if err := json.Unmarshal(body, &slots); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	if slots[0]["state"] != "active" || slots[0]["speed_mbps"] != float64(42.5) {
		t.Errorf("slot 0 = %+v, want active 42.5 mbps", slots[0])
	}
	if slots[0]["exit_ip"] != "203.0.113.1" {
		t.Errorf("slot 0 exit_ip = %v, want 203.0.113.1", slots[0]["exit_ip"])
	}
}

func TestViberoxyFetchCandidates(t *testing.T) {
	srv := fakeViberoxyControlAPI(t)
	defer srv.Close()

	client := NewViberoxyClient(srv.URL, 3*time.Second)
	body, status, err := client.FetchCandidates(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidates: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	var candidates []map[string]interface{}
	if err := json.Unmarshal(body, &candidates); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if candidates[0]["server"] != "1.2.3.4" || candidates[0]["speed_mbps"] != float64(55.2) {
		t.Errorf("candidate 0 = %+v", candidates[0])
	}
}

func TestViberoxyDropWAN(t *testing.T) {
	srv := fakeViberoxyControlAPI(t)
	defer srv.Close()

	client := NewViberoxyClient(srv.URL, 3*time.Second)

	// Successful drop.
	body, status, err := client.DropWAN(context.Background(), 0)
	if err != nil {
		t.Fatalf("DropWAN: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "replaced" {
		t.Errorf("status = %v, want replaced", resp["status"])
	}
	wan, ok := resp["wan"].(map[string]interface{})
	if !ok {
		t.Fatalf("wan missing in response")
	}
	if wan["speed_mbps"] != float64(73.5) {
		t.Errorf("wan.speed_mbps = %v, want 73.5", wan["speed_mbps"])
	}

	// Out-of-range index.
	body, status, err = client.DropWAN(context.Background(), 99)
	if err != nil {
		t.Fatalf("DropWAN(99): %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	var errResp map[string]interface{}
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp["status"] != "error" {
		t.Errorf("errResp = %+v", errResp)
	}
}

func TestViberoxyDropWAN_Unreachable(t *testing.T) {
	client := NewViberoxyClient("http://127.0.0.1:1", 500*time.Millisecond)
	_, _, err := client.DropWAN(context.Background(), 0)
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestViberoxyTriggerCycle(t *testing.T) {
	srv := fakeViberoxyControlAPI(t)
	defer srv.Close()

	client := NewViberoxyClient(srv.URL, 3*time.Second)
	body, status, err := client.TriggerCycle(context.Background())
	if err != nil {
		t.Fatalf("TriggerCycle: %v", err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %v, want accepted", resp["status"])
	}
	if resp["queued"] != true {
		t.Errorf("queued = %v, want true", resp["queued"])
	}
}

func TestViberoxyTriggerCycle_Unreachable(t *testing.T) {
	client := NewViberoxyClient("http://127.0.0.1:1", 500*time.Millisecond)
	_, _, err := client.TriggerCycle(context.Background())
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

func TestViberoxyFetchMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(testMetrics))
	}))
	defer srv.Close()

	client := NewViberoxyClient(srv.URL, 3*time.Second)
	snap, err := client.FetchMetrics(context.Background())
	if err != nil {
		t.Fatalf("FetchMetrics: %v", err)
	}

	if snap.WansActive != 2 {
		t.Errorf("WansActive = %d, want 2", snap.WansActive)
	}
	if len(snap.Slots) != 2 {
		t.Fatalf("Slots = %d, want 2", len(snap.Slots))
	}

	slot0 := snap.Slots[0]
	if slot0.Index != 0 || slot0.SpeedMbps != 42.5 || slot0.Stability != 0 {
		t.Errorf("slot0 = %+v, want index=0 speed=42.5 stability=0", slot0)
	}
	if slot0.Conns != 15 { // connect 10 + socks5 5
		t.Errorf("slot0.Conns = %d, want 15", slot0.Conns)
	}
	if slot0.ProtoConns["connect"] != 10 || slot0.ProtoConns["socks5"] != 5 {
		t.Errorf("slot0.ProtoConns = %+v", slot0.ProtoConns)
	}

	slot1 := snap.Slots[1]
	if slot1.Index != 1 || slot1.SpeedMbps != 18.2 || slot1.Stability != 2 {
		t.Errorf("slot1 = %+v, want index=1 speed=18.2 stability=2", slot1)
	}
	if slot1.Conns != 7 {
		t.Errorf("slot1.Conns = %d, want 7", slot1.Conns)
	}

	if snap.Proxy.ConnectionsTotal != 22 { // 10+5+7
		t.Errorf("ConnectionsTotal = %d, want 22", snap.Proxy.ConnectionsTotal)
	}
	if snap.Proxy.BytesUp != 3000 || snap.Proxy.BytesDown != 14000 {
		t.Errorf("bytes up/down = %d/%d, want 3000/14000", snap.Proxy.BytesUp, snap.Proxy.BytesDown)
	}

	// p50 with 20 obs: target 10, buckets 8@0.05,12@0.1 -> between 0.05-0.1
	if snap.Proxy.LatencyP50S < 0.05 || snap.Proxy.LatencyP50S > 0.1 {
		t.Errorf("LatencyP50S = %v, want in (0.05, 0.1]", snap.Proxy.LatencyP50S)
	}
	if snap.BuildVersion != "v0.1.0" {
		t.Errorf("BuildVersion = %q, want v0.1.0", snap.BuildVersion)
	}
}

func TestViberoxyFetchMetrics_Unreachable(t *testing.T) {
	client := NewViberoxyClient("http://127.0.0.1:1", 500*time.Millisecond)
	if _, err := client.FetchMetrics(context.Background()); err == nil {
		t.Error("expected error for unreachable server")
	}
}
