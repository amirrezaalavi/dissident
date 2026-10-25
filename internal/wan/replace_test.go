package wan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func TestReplaceSuccessSwapsAndDrainsOld(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 60 * time.Millisecond
	})
	old := testLink("old")
	oldProc := setActive(t, m, old, 28000)
	setReady(m, testReady("low", 1), testReady("high", 20))

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "high" {
		t.Fatalf("active = %q, want highest-score ready link", got)
	}
	if hashes := readyHashes(m); len(hashes) != 1 || hashes[0] != "low" {
		t.Fatalf("ready = %v, want [low]", hashes)
	}
	if oldProc.isStopped() {
		t.Fatalf("old process was stopped before the drain timeout")
	}
	time.Sleep(20 * time.Millisecond)
	if oldProc.isStopped() {
		t.Fatalf("old process stopped during drain window")
	}
	eventually(t, 2*time.Second, "old process drain", func() bool {
		return oldProc.isStopped()
	})
	if got := oldProc.stops(); got != 1 {
		t.Fatalf("old process stop calls = %d, want 1", got)
	}
	if got := len(fr.promotionLinks()); got != 1 || fr.promotionLinks()[0] != "high" {
		t.Fatalf("promotions = %v, want [high]", fr.promotionLinks())
	}
}

func TestReplaceFailureTriesNextReady(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old"), 28001)
	setReady(m, testReady("bad", 30), testReady("good", 5))
	store.Merge([]contract.Link{testLink("bad"), testLink("good")})

	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash == "bad" {
			return nil, errors.New("bad handshake")
		}
		return &fakeProcess{port: 28100, pid: 7700}, nil
	})

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "good" {
		t.Fatalf("active = %q, want good", got)
	}
	if hashes := readyHashes(m); len(hashes) != 0 {
		t.Fatalf("ready = %v, want empty", hashes)
	}
	if got := fr.promotionLinks(); len(got) != 2 || got[0] != "bad" || got[1] != "good" {
		t.Fatalf("promotions = %v, want [bad good]", got)
	}
	var badState contract.EntryState
	for _, e := range store.Entries() {
		if e.Link.Hash == "bad" {
			badState = e.State
		}
	}
	if badState != contract.StateFailed {
		t.Fatalf("bad catalog state = %q, want failed", badState)
	}
}

func TestReplaceFallsBackToVerificationOnlyPromotion(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	oldProc := setActive(t, m, testLink("old"), 28010)
	setReady(m, testReady("flaky-speed", 9))
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("speed endpoint timed out")
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash != "flaky-speed" {
			t.Fatalf("verification fallback got %q", link.Hash)
		}
		verified++
		return &fakeProcess{port: 28110, pid: 7800}, nil
	}

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "flaky-speed" {
		t.Fatalf("active = %q, want flaky-speed installed by fallback", got)
	}
	if verified != 1 {
		t.Fatalf("verification fallback calls = %d, want 1", verified)
	}
	if got := fr.promotionLinks(); len(got) != 1 || got[0] != "flaky-speed" {
		t.Fatalf("promotions = %v, want one full attempt", got)
	}
	eventually(t, 2*time.Second, "old process drain after fallback promotion", func() bool {
		return oldProc.isStopped()
	})
}

func TestReplaceBothPromotionPathsFailTriesNext(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old"), 28011)
	setReady(m, testReady("bad", 30), testReady("good", 5))
	store.Merge([]contract.Link{testLink("bad"), testLink("good")})

	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash == "bad" {
			return nil, errors.New("full promotion failed")
		}
		return &fakeProcess{port: 28111, pid: 7801}, nil
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		verified++
		if link.Hash == "bad" {
			return nil, errors.New("verification failed too")
		}
		return &fakeProcess{port: 28112, pid: 7802}, nil
	}

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "good" {
		t.Fatalf("active = %q, want next candidate good", got)
	}
	if verified != 1 {
		t.Fatalf("verification fallback calls = %d, want 1 (bad only)", verified)
	}
	if hashes := readyHashes(m); len(hashes) != 0 {
		t.Fatalf("ready = %v, want both candidates consumed", hashes)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "bad" && entry.State != contract.StateFailed {
			t.Fatalf("bad entry state = %q, want failed", entry.State)
		}
	}
}

func TestReplaceEmptyReadyTriggersCycle(t *testing.T) {
	m, _, store := testManager(t, nil)
	store.Merge([]contract.Link{testLink("pending")})
	err := m.Replace(context.Background())
	if err == nil {
		t.Fatal("Replace with empty ready should return an error")
	}
	eventually(t, 2*time.Second, "empty-ready cycle promotion", func() bool {
		return activeHash(m) == "pending"
	})
}

func TestReplaceEmptyReadyWithoutPendingStillErrors(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if err := m.Replace(context.Background()); err == nil {
		t.Fatal("Replace with nothing promotable should return an error")
	}
	time.Sleep(30 * time.Millisecond)
	if activeHash(m) != "" {
		t.Fatalf("active = %q, want empty", activeHash(m))
	}
}

func TestReplaceIsSerialized(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old"), 28002)
	setReady(m, testReady("cand", 5))

	var (
		mu        sync.Mutex
		inFlight  int
		maxFlight int
	)
	start := make(chan struct{})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxFlight {
			maxFlight = inFlight
		}
		mu.Unlock()
		select {
		case <-start:
		case <-ctx.Done():
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
		return &fakeProcess{port: 28200, pid: 7600}, nil
	})

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errs[i] = m.Replace(ctx)
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	close(start)
	wg.Wait()

	mu.Lock()
	gotMax := maxFlight
	mu.Unlock()
	if gotMax != 1 {
		t.Fatalf("max concurrent promotions = %d, want 1", gotMax)
	}
	if got := len(fr.promotionLinks()); got != 1 {
		t.Fatalf("promotions = %d, want 1 (only one ready candidate)", got)
	}
	// Later callers legitimately see an empty ready list and return an
	// error; only the single-winner invariant matters here.
	_ = errs
	if activeHash(m) != "cand" {
		t.Fatalf("active = %q, want cand", activeHash(m))
	}
}

func TestRemoveReady(t *testing.T) {
	m, _, _ := testManager(t, nil)
	setReady(m, testReady("a", 3), testReady("b", 2), testReady("c", 1))
	if err := m.RemoveReady("b"); err != nil {
		t.Fatalf("RemoveReady: %v", err)
	}
	if hashes := readyHashes(m); len(hashes) != 2 || hashes[0] != "a" || hashes[1] != "c" {
		t.Fatalf("ready = %v, want [a c]", hashes)
	}
	if err := m.RemoveReady("b"); err == nil {
		t.Fatal("RemoveReady on a missing hash should error")
	}
	if err := m.RemoveReady(""); err == nil {
		t.Fatal("RemoveReady with empty hash should error")
	}

	setActive(t, m, testLink("active"), 28300)
	if err := m.RemoveReady("active"); err == nil {
		t.Fatal("RemoveReady must refuse the active link")
	}

	// The removal is persisted immediately.
	in, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var saved persistedState
	if err := json.Unmarshal(in, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Ready) != 2 {
		t.Fatalf("persisted ready = %d, want 2", len(saved.Ready))
	}
}
