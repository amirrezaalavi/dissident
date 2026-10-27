package wan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"dissident/internal/contract"
)

func TestLoadStateMissingAndCorrupt(t *testing.T) {
	m, _, _ := testManager(t, nil)

	ps := m.loadState()
	if ps.Active != nil || len(ps.Ready) != 0 {
		t.Fatalf("missing state should be empty, got %+v", ps)
	}

	path := m.statePath()
	for name, data := range map[string][]byte{
		"corrupt":   []byte("{not valid json"),
		"version":   []byte(`{"version":99,"ready":[]}`),
		"truncated": []byte(`{"version":1,"active":{"link":`),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		ps = m.loadState()
		if ps.Active != nil || len(ps.Ready) != 0 {
			t.Fatalf("%s: corrupt state must be ignored, got %+v", name, ps)
		}
	}
}

func TestStateSaveLoadRoundTripAtomic(t *testing.T) {
	m, _, _ := testManager(t, nil)
	active := testLink("active-1")
	p := setActive(t, m, active, 23456)
	m.mu.Lock()
	m.active.meta.XrayPID = p.pid
	m.active.meta.ExitIP = "198.51.100.4"
	m.mu.Unlock()
	setReady(m, testReady("ready-low", 2), testReady("ready-high", 9))

	if err := m.saveState(); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	path := m.statePath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if mode := info.Mode().Perm(); runtime.GOOS != "windows" && mode != 0o600 {
		t.Fatalf("state mode = %o, want 600", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temporary state file leaked: %s", e.Name())
		}
	}

	var raw persistedState
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("state is not JSON: %v", err)
	}
	if raw.Version != stateVersion {
		t.Fatalf("version = %d, want %d", raw.Version, stateVersion)
	}
	if raw.Active == nil || raw.Active.Link.Hash != "active-1" || raw.Active.LocalPort != 23456 {
		t.Fatalf("active not persisted: %+v", raw.Active)
	}
	if len(raw.Ready) != 2 || raw.Ready[0].Link.Hash != "ready-high" {
		t.Fatalf("ready not persisted score-desc: %+v", raw.Ready)
	}
	if !raw.SavedAt.Equal(raw.SavedAt.UTC()) {
		t.Fatalf("saved_at is not UTC: %v", raw.SavedAt)
	}

	// A second save must create the best-effort backup and never leave temp
	// files or lose the atomic target.
	if err := m.saveState(); err != nil {
		t.Fatalf("second saveState: %v", err)
	}
	if _, err := os.Stat(path + stateBackupSuffix); err != nil {
		t.Fatalf("state backup missing after second save: %v", err)
	}

	m2 := New(Options{Catalog: m.catalog, Config: m.config, StateFile: path, Logger: discardLogger()})
	loaded := m2.loadState()
	if loaded.Active == nil || loaded.Active.Link.Hash != "active-1" {
		t.Fatalf("loaded active = %+v", loaded.Active)
	}
	if len(loaded.Ready) != 2 || loaded.Ready[0].Link.Hash != "ready-high" {
		t.Fatalf("loaded ready = %+v", loaded.Ready)
	}
	if loaded.Ready[0].Link.Params["uuid"] != testUUID {
		t.Fatalf("link params lost in round trip: %+v", loaded.Ready[0].Link.Params)
	}
}

func TestLoadStateDropsDuplicateReady(t *testing.T) {
	m, _, _ := testManager(t, nil)
	ps := persistedState{
		Version: stateVersion,
		Ready: []contract.Ready{
			testReady("dup", 1),
			testReady("dup", 5),
			{Link: contract.Link{Hash: ""}},
		},
	}
	data, err := json.Marshal(ps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := m.loadState()
	if len(loaded.Ready) != 1 || loaded.Ready[0].Link.Hash != "dup" {
		t.Fatalf("ready = %+v, want one deduped entry", loaded.Ready)
	}
}

func TestStartRestoresPersistedActive(t *testing.T) {
	m, fr, _ := testManager(t, nil)
	active := testLink("persisted-active")
	state := persistedState{
		Version: stateVersion,
		Active: &contract.Active{
			Link:   active,
			Since:  time.Now().Add(-time.Hour).UTC(),
			ExitIP: "203.0.113.77",
		},
		Ready: []contract.Ready{testReady("standby", 5)},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash != "persisted-active" {
			t.Fatalf("unexpected promotion in restore: %s", link.Hash)
		}
		return &fakeProcess{port: 25000, pid: 9001}, nil
	})

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := activeHash(m); got != "persisted-active" {
		t.Fatalf("active = %q, want persisted-active", got)
	}
	if m.readyCount() != 1 {
		t.Fatalf("ready = %d, want 1", m.readyCount())
	}
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if route.ID != routeIDFor("persisted-active", 9001) {
		t.Fatalf("route ID = %q", route.ID)
	}
	snap := m.Snapshot()
	if snap.Active == nil || snap.Active.LocalPort != 25000 || snap.Active.Since.IsZero() {
		t.Fatalf("snapshot active = %+v", snap.Active)
	}

	in, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("read state after restore: %v", err)
	}
	var saved persistedState
	if err := json.Unmarshal(in, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Active == nil || saved.Active.XrayPID != 9001 || saved.Active.LocalPort != 25000 {
		t.Fatalf("restored process not persisted: %+v", saved.Active)
	}
}

func TestStartClearsFailedActiveAndRunsCycle(t *testing.T) {
	m, fr, store := testManager(t, nil)
	old := testLink("old-active")
	pending := testLink("pending-new")
	store.Merge([]contract.Link{pending})
	state := persistedState{
		Version: stateVersion,
		Active:  &contract.Active{Link: old, Since: time.Now().UTC()},
	}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(m.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash == "old-active" {
			return nil, errors.New("old link is dead")
		}
		return &fakeProcess{port: 25001, pid: 9002}, nil
	})

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, "startup cycle to promote a pending link", func() bool {
		return activeHash(m) == "pending-new"
	})
	if m.LastError() == "" {
		t.Log("restore error was cleared by the successful cycle (allowed)")
	}
}

func TestStopStopsActiveAndPersistsLinks(t *testing.T) {
	m, fr, _ := testManager(t, nil)
	active := testLink("stop-active")
	p := setActive(t, m, active, 26000)
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		proc := &fakeProcess{port: 26000, pid: p.pid}
		fr.mu.Lock()
		fr.procs = append(fr.procs, proc)
		fr.mu.Unlock()
		return proc, nil
	})
	// Persist the active, then Start restores it (so Stop has a real path).
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The process created by restore must have been stopped.
	fr.mu.Lock()
	procs := append([]*fakeProcess(nil), fr.procs...)
	fr.mu.Unlock()
	if len(procs) > 0 && !procs[len(procs)-1].isStopped() {
		t.Fatalf("restored process was not stopped")
	}
	if _, err := m.Route(context.Background()); !errors.Is(err, errNoActive) {
		t.Fatalf("Route after Stop = %v, want errNoActive", err)
	}
	in, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var saved persistedState
	if err := json.Unmarshal(in, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Active == nil || saved.Active.Link.Hash != "stop-active" {
		t.Fatalf("active link not kept for restart: %+v", saved.Active)
	}
	if err := m.Start(context.Background()); !errors.Is(err, errStopped) {
		t.Fatalf("Start after Stop = %v, want errStopped", err)
	}
}
