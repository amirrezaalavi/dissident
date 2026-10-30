package wan

import (
	"testing"
	"time"

	"dissident/internal/contract"
)

func TestManagerRecentIsBoundedAndCopied(t *testing.T) {
	m, _, store := testManager(t, nil)
	now := time.Now().UTC()
	links := []contract.Link{testLink("recent-a"), testLink("recent-b"), testLink("recent-c")}
	store.Merge(links)
	store.Mark("recent-a", contract.StateCatalog, contract.Result{Hash: "recent-a", At: now.Add(-2 * time.Minute)})
	store.Mark("recent-b", contract.StateReady, contract.Result{Hash: "recent-b", OK: true, At: now})
	store.Mark("recent-c", contract.StateFailed, contract.Result{Hash: "recent-c", At: now.Add(-time.Minute)})

	recent := m.Recent(2)
	if len(recent) != 2 {
		t.Fatalf("Recent(2) = %d entries", len(recent))
	}
	if recent[0].State != contract.StateReady {
		t.Fatalf("Recent first state = %s, want ready", recent[0].State)
	}
	recent[0].Link.Params["uuid"] = "mutated"
	if got := m.Recent(2)[0].Link.Params["uuid"]; got == "mutated" {
		t.Fatal("Manager.Recent exposed stored params")
	}

	tested := m.RecentTested(1)
	if len(tested) != 1 || tested[0].Link.Hash != "recent-b" {
		t.Fatalf("RecentTested(1) = %+v", tested)
	}
	if got := m.RecentTested(10); len(got) != 3 {
		t.Fatalf("RecentTested(10) = %d entries, want 3", len(got))
	}
	if got := m.Recent(0); got != nil {
		t.Fatalf("Recent(0) = %v, want nil", got)
	}
	if got := m.RecentTested(0); got != nil {
		t.Fatalf("RecentTested(0) = %v, want nil", got)
	}
}
