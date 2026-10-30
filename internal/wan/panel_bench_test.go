package wan

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/control"
)

const panelBenchEntries = 34000

// buildPanelBenchStore fills a catalog with enough synthetic entries to
// reproduce the 34k-entry production catalog. Setup cost is excluded from the
// benchmarks.
func buildPanelBenchStore(b *testing.B) *catalog.Store {
	b.Helper()
	store := catalog.New()
	now := time.Now().UTC()
	links := make([]contract.Link, 0, panelBenchEntries)
	for i := 0; i < panelBenchEntries; i++ {
		links = append(links, contract.Link{
			Hash:     fmt.Sprintf("%064x", i+1),
			Protocol: contract.ProtocolVLESS,
			Name:     fmt.Sprintf("node-%05d", i),
			Server:   fmt.Sprintf("198.51.%d.%d", (i/250)%250, i%250+1),
			Port:     443,
			Params: map[string]string{
				"security": "tls",
				"type":     "tcp",
				"sni":      fmt.Sprintf("node-%05d.example", i),
			},
		})
	}
	if added := store.Merge(links); added != panelBenchEntries {
		b.Fatalf("merged %d entries, want %d", added, panelBenchEntries)
	}
	states := []contract.EntryState{
		contract.StateActive,
		contract.StateReady,
		contract.StateTesting,
		contract.StateCatalog,
		contract.StateFailed,
		contract.StateUnsupported,
	}
	for i, link := range links {
		state := states[i%len(states)]
		ok := state == contract.StateActive || state == contract.StateReady
		result := contract.Result{
			Hash:       link.Hash,
			Stage:      contract.StageStability,
			OK:         ok,
			Reason:     "synthetic",
			At:         now.Add(-time.Duration(i) * time.Second),
			TCPLatency: time.Duration((i%400)+1) * time.Millisecond,
			TTFB:       time.Duration((i%900)+1) * time.Millisecond,
			SpeedMbps:  float64((i % 1000) + 1),
		}
		store.Mark(link.Hash, state, result)
	}
	return store
}

func panelBenchManager(b *testing.B) *Manager {
	b.Helper()
	cfg, err := config.NewManager(config.Defaults())
	if err != nil {
		b.Fatalf("config: %v", err)
	}
	return New(Options{
		Catalog: buildPanelBenchStore(b),
		Config:  cfg,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func panelBenchHandler(b *testing.B, mgr *Manager) http.Handler {
	b.Helper()
	return control.New(control.Options{
		Data:   mgr,
		Config: testConfigManager(b),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
}

func testConfigManager(b *testing.B) *config.Manager {
	b.Helper()
	cfg := config.Defaults()
	cfg.Server.PanelListen = "127.0.0.1:0"
	mgr, err := config.NewManager(cfg)
	if err != nil {
		b.Fatalf("config: %v", err)
	}
	return mgr
}

func benchmarkPanelGET(b *testing.B, path string) {
	b.Helper()
	mgr := panelBenchManager(b)
	handler := panelBenchHandler(b, mgr)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s status = %d", path, rec.Code)
		}
	}
}

func BenchmarkPanelOverviewGET34k(b *testing.B) {
	benchmarkPanelGET(b, "/")
}

func BenchmarkPanelTestsGET34k(b *testing.B) {
	benchmarkPanelGET(b, "/tests")
}
