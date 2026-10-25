package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yolka-wiz/viber-console/internal/collector"
)

func fakeViberaydServer(t *testing.T) *httptest.Server {
	t.Helper()
	var urlsMu sync.Mutex
	urls := []string{"https://example.com/sub"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total": 120, "working": 8, "failed": 100, "unreachable": 12,
			"updated_at": "2026-08-13T09:00:00Z",
		})
	})
	mux.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"page": 1, "per_page": 50, "total": 1,
			"configs": []map[string]interface{}{
				{"host": "1.2.3.4", "port": 443, "protocol": "vless", "state": "working", "latency_ms": 120},
			},
		})
	})
	mux.HandleFunc("/api/urls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			urlsMu.Lock()
			out := append([]string(nil), urls...)
			urlsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var body struct{ URL string `json:"url"` }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			urlsMu.Lock()
			urls = append(urls, body.URL)
			urlsMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"status": "added"})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/urls/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, "/api/urls/")
		id, err := strconv.Atoi(idStr)
		if err != nil || id < 1 {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		urlsMu.Lock()
		defer urlsMu.Unlock()
		if id > len(urls) {
			http.Error(w, "out of range", http.StatusNotFound)
			return
		}
		urls = append(urls[:id-1], urls[id:]...)
		json.NewEncoder(w).Encode(map[string]string{"status": "removed"})
	})
	return httptest.NewServer(mux)
}

func fakeViberoxyServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(`# TYPE viberoxy_wans_active gauge
viberoxy_wans_active 1
# TYPE viberoxy_wan_speed_mbps gauge
viberoxy_wan_speed_mbps{index="0"} 42.5
# TYPE viberoxy_proxy_connections_total counter
viberoxy_proxy_connections_total{wan="0",proto="connect"} 10
# TYPE viberoxy_proxy_latency_seconds histogram
viberoxy_proxy_latency_seconds_bucket{le="0.1"} 5
viberoxy_proxy_latency_seconds_bucket{le="1"} 10
viberoxy_proxy_latency_seconds_bucket{le="+Inf"} 10
viberoxy_proxy_latency_seconds_sum 3
viberoxy_proxy_latency_seconds_count 10
`))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ready")) })

	mux.HandleFunc("/api/viberoxy/wans", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"index":0,"state":"active","speed_mbps":42.5,"conns":10,"exit_ip":"203.0.113.1"}]`))
	})

	mux.HandleFunc("/api/viberoxy/candidates", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"cfg1","server":"1.2.3.4","port":443,"protocol":"vless","speed_mbps":55.2}]`))
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

func newTestStore(t *testing.T) *Store {
	t.Helper()
	vdSrv := fakeViberaydServer(t)
	vxSrv := fakeViberoxyServer(t)
	t.Cleanup(vdSrv.Close)
	t.Cleanup(vxSrv.Close)

	vd := collector.NewViberaydClient(vdSrv.URL, vdSrv.URL, 2*time.Second)
	vx := collector.NewViberoxyClient(vxSrv.URL, 2*time.Second)
	store := NewStore(50*time.Millisecond, vd, vx)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go store.Run(ctx)

	// Wait for the first poll to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Get().ViberaydUp && store.Get().ViberoxyUp {
			return store
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("store never became ready")
	return nil
}

func getJSON(t *testing.T, h *Handler, path string) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

func TestHandlerOverview(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	out := getJSON(t, h, "/api/overview")

	vd := out["viberayd"].(map[string]interface{})
	if vd["reachable"] != true {
		t.Error("viberayd.reachable = false, want true")
	}
	stats := vd["stats"].(map[string]interface{})
	if stats["working"] != float64(8) {
		t.Errorf("viberayd stats.working = %v, want 8", stats["working"])
	}

	vx := out["viberoxy"].(map[string]interface{})
	if vx["reachable"] != true {
		t.Error("viberoxy.reachable = false, want true")
	}
	wans := vx["wans"].(map[string]interface{})
	if wans["active"] != float64(1) {
		t.Errorf("viberoxy wans.active = %v, want 1", wans["active"])
	}
	health := vx["health"].(map[string]interface{})
	if health["healthz"] != "ok" || health["readyz"] != "ready" {
		t.Errorf("health = %v, want ok/ready", health)
	}
}

func TestHandlerHealth(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	out := getJSON(t, h, "/api/health")
	if out["status"] != "ok" {
		t.Errorf("status = %v, want ok", out["status"])
	}
}

func TestHandlerViberaydConfigs(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	out := getJSON(t, h, "/api/viberayd/configs?page=1&per_page=50")
	if out["total"] != float64(1) {
		t.Errorf("total = %v, want 1", out["total"])
	}
	configs := out["configs"].([]interface{})
	if len(configs) != 1 {
		t.Fatalf("configs len = %d, want 1", len(configs))
	}
}

func TestHandlerViberaydURLs(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	out := getJSON(t, h, "/api/viberayd/urls")
	urls := out["urls"].([]interface{})
	if len(urls) != 1 || urls[0] != "https://example.com/sub" {
		t.Errorf("urls = %v, want [https://example.com/sub]", urls)
	}
}

func TestHandlerViberaydURLsPut(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	// PUT a new full list (the fake viberayd accepts POST/DELETE and the
	// store's diff-based replace applies it).
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]interface{}{
		"urls": []string{
			"https://example.com/sub",
			"https://second.example/sub",
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/viberayd/urls", &buf)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT urls = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	urls := out["urls"].([]interface{})
	if len(urls) != 2 {
		t.Errorf("urls after PUT = %v, want 2", urls)
	}
}

func TestHandlerViberaydURLsPutInvalid(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(map[string]interface{}{
		"urls": []string{
			"https://good.example/sub",
			"not-a-url",
			"ftp://bad.example/x",
		},
	})
	req := httptest.NewRequest(http.MethodPut, "/api/viberayd/urls", &buf)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid urls = %d, want 400", rec.Code)
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	invalid := out["invalid"].([]interface{})
	if len(invalid) != 2 {
		t.Errorf("invalid lines = %v, want 2 rejected", invalid)
	}
}

func TestValidateURLList(t *testing.T) {
	valid, invalid := validateURLList([]string{
		"  https://a.example/sub  ",
		"http://b.example",
		"# comment",
		"",
		"not-a-url",
		"ftp://c.example",
	})
	if len(valid) != 2 {
		t.Errorf("valid = %v, want 2", valid)
	}
	if valid[0] != "https://a.example/sub" {
		t.Errorf("valid[0] = %q, want trimmed url", valid[0])
	}
	if len(invalid) != 2 {
		t.Errorf("invalid = %v, want 2", invalid)
	}
}

func TestHandlerViberoxyMetrics(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	out := getJSON(t, h, "/api/viberoxy/metrics")
	if out["wans_active"] != float64(1) {
		t.Errorf("wans_active = %v, want 1", out["wans_active"])
	}
}

func TestHandlerOverview_DaemonDown(t *testing.T) {
	// A store with an unreachable viberoxy should still serve the overview
	// with reachable=false instead of erroring.
	vdSrv := fakeViberaydServer(t)
	t.Cleanup(vdSrv.Close)
	vd := collector.NewViberaydClient(vdSrv.URL, vdSrv.URL, 2*time.Second)
	vx := collector.NewViberoxyClient("http://127.0.0.1:1", 200*time.Millisecond)
	store := NewStore(50*time.Millisecond, vd, vx)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go store.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := store.Get()
		if s.ViberaydUp && !s.ViberoxyUp {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	h := NewHandler(store)
	out := getJSON(t, h, "/api/overview")
	vxOut := out["viberoxy"].(map[string]interface{})
	if vxOut["reachable"] != false {
		t.Error("viberoxy.reachable = true, want false (daemon down)")
	}
}

func TestHandlerWANSlots(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/viberoxy/wans", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/viberoxy/wans = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var rawSlots []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawSlots); err != nil {
		t.Fatalf("decode raw response: %v (body=%s)", err, rec.Body.String())
	}
	if len(rawSlots) != 1 {
		t.Fatalf("got %d slots, want 1", len(rawSlots))
	}
	if rawSlots[0]["state"] != "active" {
		t.Errorf("slot 0 state = %v, want active", rawSlots[0]["state"])
	}
	if rawSlots[0]["exit_ip"] != "203.0.113.1" {
		t.Errorf("slot 0 exit_ip = %v, want 203.0.113.1", rawSlots[0]["exit_ip"])
	}
}

func TestHandlerCandidates(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/viberoxy/candidates", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/viberoxy/candidates = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var candidates []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &candidates); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0]["server"] != "1.2.3.4" {
		t.Errorf("candidate 0 server = %v, want 1.2.3.4", candidates[0]["server"])
	}
}

func TestHandlerDropWAN(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodPost, "/api/viberoxy/wans/0/drop", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/viberoxy/wans/0/drop = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
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
}

func TestHandlerDropWAN_InvalidIndex(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodPost, "/api/viberoxy/wans/abc/drop", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/viberoxy/wans/abc/drop = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestHandlerDropWAN_MethodNotAllowed(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/viberoxy/wans/0/drop", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/viberoxy/wans/0/drop = %d, want 405", rec.Code)
	}
}

func TestHandlerTriggerCycle(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodPost, "/api/viberoxy/cycle/trigger", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/viberoxy/cycle/trigger = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("status = %v, want accepted", resp["status"])
	}
}

func TestHandlerTriggerCycle_MethodNotAllowed(t *testing.T) {
	store := newTestStore(t)
	h := NewHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/api/viberoxy/cycle/trigger", nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/viberoxy/cycle/trigger = %d, want 405", rec.Code)
	}
}
