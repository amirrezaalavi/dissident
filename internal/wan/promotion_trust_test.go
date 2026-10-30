package wan

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

// freshReady returns a ready entry whose full Test pass is inside the trust
// window.
func freshReady(hash string, score float64) contract.Ready {
	r := testReady(hash, score)
	r.TestedAt = time.Now().UTC()
	return r
}

func TestReplaceFreshReadyUsesTrustedPromotion(t *testing.T) {
	m, fr, _ := testManager(t, nil)
	setActive(t, m, testLink("old"), 28500)
	setReady(m, freshReady("fresh", 10))

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "fresh" {
		t.Fatalf("active = %q, want fresh", got)
	}
	if got := fr.promotionLinks(); len(got) != 0 {
		t.Fatalf("full Promote calls = %v, want none for a fresh ready entry", got)
	}
	if got := fr.trustedLinks(); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("trusted promotions = %v, want [fresh]", got)
	}
}

func TestReplaceStaleOrMissingTestedAtUsesFullPromotion(t *testing.T) {
	for name, testedAt := range map[string]time.Time{
		"zero":  {},
		"stale": time.Now().Add(-promotionTrustWindow - time.Minute).UTC(),
	} {
		t.Run(name, func(t *testing.T) {
			m, fr, _ := testManager(t, nil)
			setActive(t, m, testLink("old"), 28501)
			cand := testReady("old-evidence", 10)
			cand.TestedAt = testedAt
			setReady(m, cand)

			if err := m.Replace(context.Background()); err != nil {
				t.Fatalf("Replace: %v", err)
			}
			if got := activeHash(m); got != "old-evidence" {
				t.Fatalf("active = %q, want old-evidence", got)
			}
			if got := fr.trustedLinks(); len(got) != 0 {
				t.Fatalf("trusted promotions = %v, want none", got)
			}
			if got := fr.promotionLinks(); len(got) != 1 || got[0] != "old-evidence" {
				t.Fatalf("full promotions = %v, want [old-evidence]", got)
			}
		})
	}
}

func TestTrustedPromotionFailureDropsCandidateWithoutFallback(t *testing.T) {
	m, fr, store := testManager(t, nil)
	oldProc := setActive(t, m, testLink("old"), 28502)
	setReady(m, freshReady("trust-fail", 10))
	store.Merge([]contract.Link{testLink("trust-fail")})

	fr.setTrustedFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("verify failed")
	})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("full promotion must not run")
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		verified++
		return &fakeProcess{port: 28503, pid: 7000}, nil
	}

	if err := m.Replace(context.Background()); err == nil {
		t.Fatal("Replace should fail when the trusted path fails")
	}
	if got := activeHash(m); got != "old" {
		t.Fatalf("active = %q, want old preserved", got)
	}
	if oldProc.isStopped() {
		t.Fatal("old active was stopped by a failed trusted promotion")
	}
	if got := fr.promotionLinks(); len(got) != 0 {
		t.Fatalf("full promotions = %v, want none after trusted failure", got)
	}
	if verified != 0 {
		t.Fatalf("verification fallback calls = %d, want 0", verified)
	}
	if hashes := readyHashes(m); len(hashes) != 0 {
		t.Fatalf("ready = %v, want failed candidate dropped", hashes)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "trust-fail" && entry.State != contract.StateFailed {
			t.Fatalf("catalog state = %q, want failed", entry.State)
		}
	}
}

func TestTrustedPromotionKeepsMakeBeforeBreakDrain(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		// Wide enough that scheduler jitter after the 20ms check cannot make
		// the drain timer look premature on a loaded builder.
		c.Swap.DrainTimeout = 300 * time.Millisecond
	})
	oldProc := setActive(t, m, testLink("old"), 28504)
	setReady(m, freshReady("fresh-drain", 10))

	if err := m.Replace(context.Background()); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := activeHash(m); got != "fresh-drain" {
		t.Fatalf("active = %q, want fresh-drain", got)
	}
	if oldProc.isStopped() {
		t.Fatal("old process was stopped before the drain timeout")
	}
	time.Sleep(20 * time.Millisecond)
	if oldProc.isStopped() {
		t.Fatal("old process stopped during the drain window")
	}
	eventually(t, 2*time.Second, "old process drain after trusted swap", oldProc.isStopped)
}

func TestCycleFreshTestPromotesTrusted(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
	})
	store.Merge([]contract.Link{testLink("just-tested")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got != "just-tested" {
		t.Fatalf("active = %q, want just-tested", got)
	}
	if got := fr.promotionLinks(); len(got) != 0 {
		t.Fatalf("full promotions = %v, want none immediately after a full Test pass", got)
	}
	if got := fr.trustedLinks(); len(got) != 1 || got[0] != "just-tested" {
		t.Fatalf("trusted promotions = %v, want [just-tested]", got)
	}
}

func TestCycleTrustedFailureDoesNotInstallOrFallBack(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
	})
	store.Merge([]contract.Link{testLink("trust-fail-cycle")})
	fr.setTrustedFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("verify timed out")
	})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("full promotion must not run")
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		verified++
		return &fakeProcess{port: 28505, pid: 7001}, nil
	}

	if err := m.RunCycle(context.Background()); err == nil {
		t.Fatal("RunCycle with a failed trusted promotion should report no active WAN")
	}
	if got := activeHash(m); got != "" {
		t.Fatalf("active = %q, want empty", got)
	}
	if got := fr.promotionLinks(); len(got) != 0 {
		t.Fatalf("full promotions = %v, want none", got)
	}
	if got := fr.trustedLinks(); len(got) != 1 {
		t.Fatalf("trusted promotions = %v, want one attempt", got)
	}
	if verified != 0 {
		t.Fatalf("verification fallback calls = %d, want 0", verified)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "trust-fail-cycle" && entry.State != contract.StateFailed {
			t.Fatalf("catalog state = %q, want failed", entry.State)
		}
	}
}

func TestTestedAtSurvivesStateAndIsNotRefreshedByLiveness(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 3
	})
	cand := readyFor("persisted-ready", "ready.example", 443, 4)
	testedAt := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	cand.TestedAt = testedAt
	setReady(m, cand)
	store.Merge([]contract.Link{cand.Link})

	if err := m.saveState(); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	reloaded := New(Options{
		Catalog:   m.catalog,
		Config:    m.config,
		StateFile: m.statePath(),
		Logger:    discardLogger(),
	}).loadState()
	if len(reloaded.Ready) != 1 {
		t.Fatalf("reloaded ready = %+v, want one entry", reloaded.Ready)
	}
	if !reloaded.Ready[0].TestedAt.Equal(testedAt) {
		t.Fatalf("TestedAt after reload = %v, want %v", reloaded.Ready[0].TestedAt, testedAt)
	}

	// Ready liveness refreshes LastCheck only. TestedAt is full-Test evidence
	// and must remain untouched.
	oldCheck := cand.LastCheck
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		return pipeConn(), nil
	}
	m.checkReadyOnce(context.Background())

	m.mu.Lock()
	got := m.ready[0]
	m.mu.Unlock()
	if !got.TestedAt.Equal(testedAt) {
		t.Fatalf("TestedAt after liveness = %v, want %v", got.TestedAt, testedAt)
	}
	if !got.LastCheck.After(oldCheck) {
		t.Fatalf("LastCheck after liveness = %v, want after %v", got.LastCheck, oldCheck)
	}
}
