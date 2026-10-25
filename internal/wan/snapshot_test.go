package wan

import (
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func TestSnapshotAndEntriesAreCopies(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	active := testLink("snap-active")
	setActive(t, m, active, 30000)
	setReady(m, testReady("snap-ready", 4))
	store.Merge([]contract.Link{testLink("entry-one")})
	store.Mark("entry-one", contract.StateFailed, contract.Result{
		Hash: "entry-one", Stage: contract.StageTCP, Reason: "nope", At: time.Now().UTC(),
	})

	snap := m.Snapshot()
	if snap.Generation != 1 {
		t.Fatalf("generation = %d, want 1", snap.Generation)
	}
	if snap.Active == nil || snap.Active.Link.Hash != "snap-active" {
		t.Fatalf("active = %+v", snap.Active)
	}
	if len(snap.Ready) != 1 || snap.Ready[0].Link.Hash != "snap-ready" {
		t.Fatalf("ready = %+v", snap.Ready)
	}
	if snap.CatalogCount != 1 || snap.SupportedCount != 1 || snap.Testing != 0 {
		t.Fatalf("catalog counts = %d/%d/%d", snap.CatalogCount, snap.SupportedCount, snap.Testing)
	}
	if snap.Updated.IsZero() || !snap.Updated.Equal(snap.Updated.UTC()) {
		t.Fatalf("updated = %v, want UTC", snap.Updated)
	}

	// Mutating the returned snapshot must not affect the manager.
	snap.Active.Link.Params["uuid"] = "mutated"
	snap.Ready[0].Link.Params["uuid"] = "mutated"
	again := m.Snapshot()
	if got := again.Active.Link.Params["uuid"]; got != testUUID {
		t.Fatalf("snapshot active params leaked: %q", got)
	}
	if got := again.Ready[0].Link.Params["uuid"]; got != testUUID {
		t.Fatalf("snapshot ready params leaked: %q", got)
	}

	entries := m.Entries()
	if len(entries) != 1 || entries[0].Link.Hash != "entry-one" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Last.Reason != "nope" {
		t.Fatalf("entry result = %+v", entries[0].Last)
	}
	entries[0].Link.Params["uuid"] = "mutated"
	entries[0].Last.Reason = "mutated"
	againEntries := m.Entries()
	if againEntries[0].Link.Params["uuid"] != testUUID || againEntries[0].Last.Reason != "nope" {
		t.Fatalf("Entries leaked mutable values: %+v", againEntries[0])
	}
}

func TestSnapshotNoActiveOrReady(t *testing.T) {
	m, _, _ := testManager(t, nil)
	snap := m.Snapshot()
	if snap.Active != nil {
		t.Fatalf("active = %+v, want nil", snap.Active)
	}
	if len(snap.Ready) != 0 {
		t.Fatalf("ready = %+v, want empty", snap.Ready)
	}
	if snap.Generation != 1 || snap.Updated.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}
}
