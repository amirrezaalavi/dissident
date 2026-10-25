package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yolka-wiz/viber-console/internal/collector"
	"github.com/yolka-wiz/viber-console/internal/config"
	"github.com/yolka-wiz/viber-console/internal/supervisor"
)

func newControlHandler(t *testing.T, cfgDir string) (*ControlHandler, *Store) {
	t.Helper()
	vdSrv := fakeViberaydServer(t)
	vxSrv := fakeViberoxyServer(t)
	t.Cleanup(vdSrv.Close)
	t.Cleanup(vxSrv.Close)

	vd := collectorClient(vdSrv.URL)
	vx := collectorClientVX(vxSrv.URL)
	store := NewStore(50*time.Millisecond, vd, vx)
	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	go store.Run(ctx)

	cfgStore := config.NewStore(cfgDir)
	return NewControlHandler(store, cfgStore, nil, vdSrv.URL, ""), store
}

// collectorClient builds a viberayd client against a fake server URL.
func collectorClient(apiURL string) *collector.ViberaydClient {
	return collector.NewViberaydClient(apiURL, apiURL, 2*time.Second)
}

// collectorClientVX builds a viberoxy client against a fake server URL.
func collectorClientVX(url string) *collector.ViberoxyClient {
	return collector.NewViberoxyClient(url, 2*time.Second)
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// fakeSleeper writes a tiny shell script that sleeps, for process tests.
func fakeSleeper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sleeper")
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func doJSON(t *testing.T, h *ControlHandler, method, path string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Routes(mux)
	mux.ServeHTTP(rec, req)

	out := map[string]interface{}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestControlSchema(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "GET", "/api/config/schema", nil)
	if code != http.StatusOK {
		t.Fatalf("schema = %d, want 200", code)
	}
	groups := out["groups"].(map[string]interface{})
	if _, ok := groups["viberayd"]; !ok {
		t.Error("missing viberayd group")
	}
	if _, ok := groups["viberoxy"]; !ok {
		t.Error("missing viberoxy group")
	}
}

func TestControlValuesGet(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "GET", "/api/config/values", nil)
	if code != http.StatusOK {
		t.Fatalf("values = %d, want 200", code)
	}
	if _, ok := out["viberayd"]; !ok {
		t.Error("missing viberayd values")
	}
	if _, ok := out["viberoxy"]; !ok {
		t.Error("missing viberoxy values")
	}
}

func TestControlValuesPost(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "POST", "/api/config/values", map[string]interface{}{
		"group":  "viberoxy",
		"values": map[string]string{"WAN_COUNT": "3", "ROUTE_MODE": "proxy-default"},
	})
	if code != http.StatusOK {
		t.Fatalf("post = %d, want 200 (body %v)", code, out)
	}
	if out["saved"] != true {
		t.Error("saved != true")
	}
}

func TestControlValuesPostInvalid(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "POST", "/api/config/values", map[string]interface{}{
		"group":  "viberoxy",
		"values": map[string]string{"WAN_COUNT": "99"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid post = %d, want 400", code)
	}
	if out["error"] == "" {
		t.Error("expected error message")
	}
}

func TestControlValuesPostBadGroup(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, _ := doJSON(t, h, "POST", "/api/config/values", map[string]interface{}{
		"group": "nope",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad group = %d, want 400", code)
	}
}

func TestControlAuth(t *testing.T) {
	vdSrv := fakeViberaydServer(t)
	t.Cleanup(vdSrv.Close)
	cfgStore := config.NewStore(t.TempDir())
	h := NewControlHandler(nil, cfgStore, nil, vdSrv.URL, "sekret")

	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest("GET", "/api/config/schema", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/config/schema", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/config/schema", nil)
	req.Header.Set("X-Console-Token", "sekret")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-token = %d, want 200", rec.Code)
	}
}

func TestControlRestartUnknownService(t *testing.T) {
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "POST", "/api/control/restart", map[string]interface{}{
		"service": "nonexistent",
	})
	if code != http.StatusOK {
		t.Fatalf("restart = %d, want 200 (no-op)", code)
	}
	if len(out["restarted"].([]interface{})) != 0 {
		t.Error("expected no restarted services")
	}
}

func TestControlProcesses(t *testing.T) {
	// With no services, processes returns an empty list.
	h, _ := newControlHandler(t, t.TempDir())
	code, out := doJSON(t, h, "GET", "/api/processes", nil)
	if code != http.StatusOK {
		t.Fatalf("processes = %d, want 200", code)
	}
	svcs := out["services"].([]interface{})
	if len(svcs) != 0 {
		t.Errorf("services = %d, want 0", len(svcs))
	}
}

func TestControlProcessesWithService(t *testing.T) {
	// Use a fake service whose binary is a sleeper to exercise Status().
	bin := fakeSleeper(t)
	svc := supervisor.NewService("viberoxy", bin, "")
	ctx, cancel := contextWithCancel()
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}

	vdSrv := fakeViberaydServer(t)
	t.Cleanup(vdSrv.Close)
	cfgStore := config.NewStore(t.TempDir())
	h := NewControlHandler(nil, cfgStore, []*supervisor.Service{svc}, vdSrv.URL, "")

	code, out := doJSON(t, h, "GET", "/api/processes", nil)
	if code != http.StatusOK {
		t.Fatalf("processes = %d, want 200", code)
	}
	svcs := out["services"].([]interface{})
	if len(svcs) != 1 {
		t.Fatalf("services = %d, want 1", len(svcs))
	}
	first := svcs[0].(map[string]interface{})
	if first["running"] != true || first["pid"] == nil {
		t.Errorf("service status = %v, want running with pid", first)
	}
	svc.Stop()
}
