package wan

import (
	"context"
	"errors"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

// installForTest installs one ready candidate through the same path as a real
// swap so the post-install passive grace is armed.
func installForTest(t *testing.T, m *Manager, cand contract.Ready, port int) *fakeProcess {
	t.Helper()
	p := &fakeProcess{port: port, pid: port + 1000}
	m.replaceMu.Lock()
	err := m.installActive(cand, p)
	m.replaceMu.Unlock()
	if err != nil {
		t.Fatalf("installActive: %v", err)
	}
	return p
}

func TestPassiveFailuresInsideGraceDoNotReplace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeWindow = 200 * time.Millisecond
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	m.mu.Lock()
	m.passiveGrace = 200 * time.Millisecond
	m.mu.Unlock()

	first := testReady("first", 10)
	second := testReady("second", 5)
	setReady(m, first, second)
	installForTest(t, m, first, 27200)
	setReady(m, second)

	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	for i := 0; i < passiveFailureThreshold; i++ {
		m.Report(context.Background(), route, errTest)
	}
	time.Sleep(30 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("%d passive failures inside the grace triggered %d promotions, want 0", passiveFailureThreshold, got)
	}
	if got := activeHash(m); got != "first" {
		t.Fatalf("active = %q, want first route preserved during grace", got)
	}

	// Wait out the grace, then a fresh failure window must trigger exactly one
	// replacement.
	time.Sleep(220 * time.Millisecond)
	for i := 0; i < passiveFailureThreshold; i++ {
		m.Report(context.Background(), route, errTest)
	}
	eventually(t, 2*time.Second, "replacement after passive grace", func() bool {
		return activeHash(m) == "second"
	})
	if got := len(fr.promotionLinks()); got != 1 {
		t.Fatalf("promotions = %d, want exactly 1 after grace", got)
	}
}

func TestPassiveReplacementFailureDoesNotStorm(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeWindow = 500 * time.Millisecond
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	m.mu.Lock()
	m.passiveCooldown = time.Hour
	m.mu.Unlock()

	setActive(t, m, testLink("old-route"), 27201)
	setReady(m, testReady("bad-candidate", 9))
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("promotion failed")
	})
	route, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	for i := 0; i < passiveFailureThreshold; i++ {
		m.Report(context.Background(), route, errTest)
	}
	eventually(t, 2*time.Second, "first passive replacement attempt", func() bool {
		return len(fr.promotionLinks()) == 1 && !m.replaceScheduled.Load()
	})
	if got := activeHash(m); got != "old-route" {
		t.Fatalf("active = %q, want old route after failed replacement", got)
	}

	// replaceScheduled is clear again; only the passive cooldown may stop a
	// second storm of replacement attempts.
	for i := 0; i < passiveFailureThreshold; i++ {
		m.Report(context.Background(), route, errTest)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 1 {
		t.Fatalf("promotions = %d after repeated failures, want 1 (cooldown)", got)
	}
}

func TestLivenessReplacesDuringPassiveGrace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ProbeCount = 1
		c.Cycle.ProbeBudgetPerMin = 30
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("dead-route"), 27202)
	m.mu.Lock()
	// Simulate an active route that was just installed: passive failures are
	// in their grace window, but liveness must still detect a dead route.
	m.nextPassiveReplaceAt = time.Now().Add(time.Hour)
	m.mu.Unlock()
	setReady(m, testReady("live-candidate", 9))
	fr.setProbeFn(func(ctx context.Context, addr string) error { return errTest })

	m.probeOnce(context.Background())
	eventually(t, 2*time.Second, "liveness replacement during passive grace", func() bool {
		return activeHash(m) == "live-candidate"
	})
}
