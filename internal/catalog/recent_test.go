package catalog

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"dissident/internal/contract"
)

func recentTestEntry(hash string, state contract.EntryState, updated time.Time, score float64) *contract.Entry {
	return &contract.Entry{
		Link: contract.Link{
			Hash:     hash,
			Name:     "entry-" + hash,
			Protocol: contract.ProtocolVLESS,
			Params:   map[string]string{"security": "tls", "tag": hash},
		},
		State:   state,
		Score:   score,
		Updated: updated,
	}
}

func recentTestHashes(entries []contract.Entry) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.Link.Hash
	}
	return out
}

func TestStoreRecentLimitAndOrdering(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := New()
	store.entries = map[string]*contract.Entry{
		"testing-old":   recentTestEntry("testing-old", contract.StateTesting, base.Add(-5*time.Hour), 1),
		"active-old":    recentTestEntry("active-old", contract.StateActive, base.Add(-4*time.Hour), 1),
		"ready-old":     recentTestEntry("ready-old", contract.StateReady, base.Add(-3*time.Hour), 1),
		"catalog-newer": recentTestEntry("catalog-newer", contract.StateCatalog, base, 1),
		"catalog-high":  recentTestEntry("catalog-high", contract.StateCatalog, base.Add(-time.Hour), 20),
		"catalog-low":   recentTestEntry("catalog-low", contract.StateCatalog, base.Add(-time.Hour), 10),
		"catalog-aa":    recentTestEntry("catalog-aa", contract.StateCatalog, base.Add(-time.Hour), 10),
		"catalog-bb":    recentTestEntry("catalog-bb", contract.StateCatalog, base.Add(-time.Hour), 10),
		"failed-newest": recentTestEntry("failed-newest", contract.StateFailed, base.Add(time.Hour), 1),
	}

	want := []string{
		"testing-old", "active-old", "ready-old",
		"catalog-newer", "catalog-high", "catalog-aa", "catalog-bb", "catalog-low",
		"failed-newest",
	}
	got := recentTestHashes(store.Recent(20))
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Recent order = %v, want %v", got, want)
	}

	limited := store.Recent(3)
	if fmt.Sprint(recentTestHashes(limited)) != fmt.Sprint(want[:3]) {
		t.Fatalf("Recent(3) = %v, want %v", recentTestHashes(limited), want[:3])
	}
	if recent := store.Recent(0); recent != nil {
		t.Fatalf("Recent(0) = %v, want nil", recent)
	}
	if recent := store.Recent(-1); recent != nil {
		t.Fatalf("Recent(-1) = %v, want nil", recent)
	}
}

func TestStoreRecentTestedOrdering(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := New()
	untested := recentTestEntry("untested", contract.StateCatalog, base, 0)
	untested.Last.At = time.Time{}
	store.entries = map[string]*contract.Entry{
		"old":      recentEntryWithResult("old", base.Add(-time.Hour)),
		"new":      recentEntryWithResult("new", base),
		"middle":   recentEntryWithResult("middle", base.Add(-time.Minute)),
		"aa":       recentEntryWithResult("aa", base.Add(-time.Minute)),
		"zz":       recentEntryWithResult("zz", base.Add(-time.Minute)),
		"untested": untested,
	}

	all := store.RecentTested(10)
	if len(all) != 5 {
		t.Fatalf("RecentTested returned %d entries, want 5", len(all))
	}
	want := []string{"new", "aa", "middle", "zz", "old"}
	if got := fmt.Sprint(recentTestHashes(all)); got != fmt.Sprint(want) {
		t.Fatalf("RecentTested order = %s, want %v", got, want)
	}

	limited := store.RecentTested(2)
	if got := fmt.Sprint(recentTestHashes(limited)); got != fmt.Sprint(want[:2]) {
		t.Fatalf("RecentTested(2) = %s, want %v", got, want[:2])
	}
	if tested := store.RecentTested(0); tested != nil {
		t.Fatalf("RecentTested(0) = %v, want nil", tested)
	}
}

func recentEntryWithResult(hash string, at time.Time) *contract.Entry {
	entry := recentTestEntry(hash, contract.StateCatalog, at, 1)
	entry.Last = contract.Result{Hash: hash, Stage: contract.StageTCP, OK: true, At: at}
	return entry
}

func TestStoreRecentDoesNotAliasStoredEntries(t *testing.T) {
	store := New()
	stored := recentTestEntry("hash-one", contract.StateReady, time.Now().UTC(), 5)
	stored.Last = contract.Result{Hash: "hash-one", OK: true, Reason: "stored", At: time.Now().UTC()}
	store.entries = map[string]*contract.Entry{"hash-one": stored}

	recent := store.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("Recent = %d entries", len(recent))
	}
	recent[0].Link.Params["tag"] = "mutated"
	recent[0].Last.Reason = "mutated"
	recent[0].Link.Name = "mutated"

	again := store.Recent(1)
	if got := again[0].Link.Params["tag"]; got != "hash-one" {
		t.Fatalf("Recent exposed stored params: %q", got)
	}
	if again[0].Link.Name != "entry-hash-one" {
		t.Fatalf("Recent exposed stored link name: %q", again[0].Link.Name)
	}
	if again[0].Last.Reason != "stored" {
		t.Fatalf("Recent exposed stored result: %q", again[0].Last.Reason)
	}

	tested := store.RecentTested(1)
	tested[0].Link.Params["tag"] = "mutated"
	if got := store.RecentTested(1)[0].Link.Params["tag"]; got != "hash-one" {
		t.Fatalf("RecentTested exposed stored params: %q", got)
	}
}

func TestStoreRecentMatchesFullSelection(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	states := []contract.EntryState{
		contract.StateTesting,
		contract.StateActive,
		contract.StateReady,
		contract.StateCatalog,
		contract.StateFailed,
		contract.StateUnsupported,
	}
	store := New()
	store.entries = make(map[string]*contract.Entry, 1000)
	var all []*contract.Entry
	var tested []*contract.Entry
	for i := 0; i < 1000; i++ {
		hash := fmt.Sprintf("%064x", (i*7919)%1000+1)
		entry := recentTestEntry(hash, states[i%len(states)],
			base.Add(-time.Duration((i*37)%1000)*time.Millisecond), float64((i*13)%17))
		if i%3 != 0 {
			entry.Last = contract.Result{
				Hash: hash,
				At:   base.Add(-time.Duration((i*53)%1000) * time.Millisecond),
			}
			tested = append(tested, entry)
		}
		store.entries[hash] = entry
		all = append(all, entry)
	}

	reference := func(entries []*contract.Entry, testedOnly bool, limit int) []string {
		cp := append([]*contract.Entry(nil), entries...)
		sort.Slice(cp, func(i, j int) bool {
			return recentBefore(cp[i], cp[j], testedOnly)
		})
		if len(cp) > limit {
			cp = cp[:limit]
		}
		out := make([]string, len(cp))
		for i, entry := range cp {
			out[i] = entry.Link.Hash
		}
		return out
	}

	for _, limit := range []int{1, 10, 200, 999, 1000, 2000} {
		want := reference(all, false, limit)
		got := recentTestHashes(store.Recent(limit))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("Recent(%d) = %v, want %v", limit, got, want)
		}
		want = reference(tested, true, limit)
		got = recentTestHashes(store.RecentTested(limit))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("RecentTested(%d) = %v, want %v", limit, got, want)
		}
	}
}

func TestStoreRecentConcurrentUse(t *testing.T) {
	store := New()
	links := make([]contract.Link, 0, 500)
	for i := 0; i < 500; i++ {
		links = append(links, contract.Link{
			Hash:     fmt.Sprintf("%064x", i+1),
			Protocol: contract.ProtocolVLESS,
			Name:     fmt.Sprintf("node-%03d", i),
			Server:   "198.51.100.10",
			Port:     443,
			Params:   map[string]string{"tag": fmt.Sprint(i)},
		})
	}
	store.Merge(links)
	for i, link := range links {
		state := contract.StateCatalog
		if i%3 == 0 {
			state = contract.StateReady
		}
		store.Mark(link.Hash, state, contract.Result{
			Hash: link.Hash,
			OK:   i%2 == 0,
			At:   time.Now().UTC().Add(-time.Duration(i) * time.Second),
		})
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if got := store.Recent(10); len(got) > 10 {
					t.Errorf("worker %d: Recent returned %d entries", worker, len(got))
					return
				}
				if got := store.RecentTested(10); len(got) > 10 {
					t.Errorf("worker %d: RecentTested returned %d entries", worker, len(got))
					return
				}
				link := links[(worker*100+i)%len(links)]
				store.Mark(link.Hash, contract.StateTesting, contract.Result{
					Hash: link.Hash,
					At:   time.Now().UTC(),
				})
			}
		}(worker)
	}
	wg.Wait()
}

func TestStoreRecentKeepsExcludedVisible(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := New()
	store.entries = make(map[string]*contract.Entry)
	for i := 0; i < 500; i++ {
		hash := fmt.Sprintf("catalog-%03d", i)
		store.entries[hash] = recentTestEntry(hash, contract.StateCatalog, base.Add(-time.Duration(i)*time.Minute), 1)
	}
	store.entries["excluded-old"] = recentTestEntry("excluded-old", contract.StateExcluded, base.Add(-100*time.Hour), 1)

	found := false
	for _, entry := range store.Recent(50) {
		if entry.Link.Hash == "excluded-old" {
			found = true
		}
	}
	if !found {
		t.Fatal("operator-excluded entry was pushed out of the bounded recent list")
	}
}
