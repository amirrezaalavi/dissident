package catalog

import (
	"fmt"
	"testing"
	"time"

	"dissident/internal/contract"
)

const recentBenchEntries = 30000

func recentBenchStore(b *testing.B) *Store {
	b.Helper()
	now := time.Now().UTC()
	states := []contract.EntryState{
		contract.StateTesting,
		contract.StateActive,
		contract.StateReady,
		contract.StateCatalog,
		contract.StateFailed,
		contract.StateUnsupported,
	}
	store := New()
	store.entries = make(map[string]*contract.Entry, recentBenchEntries)
	for i := 0; i < recentBenchEntries; i++ {
		hash := fmt.Sprintf("%064x", i+1)
		entry := &contract.Entry{
			Link: contract.Link{
				Hash:     hash,
				Protocol: contract.ProtocolVLESS,
				Name:     fmt.Sprintf("node-%05d", i),
				Server:   fmt.Sprintf("198.51.%d.%d", (i/250)%250, i%250+1),
				Port:     443,
				Params: map[string]string{
					"security": "tls",
					"type":     "tcp",
					"sni":      fmt.Sprintf("node-%05d.example", i),
				},
			},
			State:   states[i%len(states)],
			Score:   float64((i % 1000) + 1),
			Updated: now.Add(-time.Duration(i) * time.Second),
		}
		if i%2 == 0 {
			entry.Last = contract.Result{
				Hash:       hash,
				Stage:      contract.StageTCP,
				OK:         true,
				At:         now.Add(-time.Duration(i) * time.Second),
				TCPLatency: time.Duration((i%400)+1) * time.Millisecond,
				SpeedMbps:  float64((i % 1000) + 1),
			}
		}
		store.entries[hash] = entry
	}
	return store
}

func BenchmarkStoreRecent10(b *testing.B) {
	store := recentBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.Recent(10); len(got) != 10 {
			b.Fatalf("Recent(10) = %d entries", len(got))
		}
	}
}

func BenchmarkStoreRecentTested10(b *testing.B) {
	store := recentBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.RecentTested(10); len(got) != 10 {
			b.Fatalf("RecentTested(10) = %d entries", len(got))
		}
	}
}

func BenchmarkStoreRecent200(b *testing.B) {
	store := recentBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.Recent(200); len(got) != 200 {
			b.Fatalf("Recent(200) = %d entries", len(got))
		}
	}
}

// BenchmarkStoreEntries30k records the old full-catalog copy and sort path for
// comparison with the bounded Recent methods.
func BenchmarkStoreEntries30k(b *testing.B) {
	store := recentBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := store.Entries(); len(got) != recentBenchEntries {
			b.Fatalf("Entries() = %d entries", len(got))
		}
	}
}
