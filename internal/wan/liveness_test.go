package wan

import (
	"context"
	"testing"
	"time"

	"dissident/internal/config"
)

func TestProbeFailuresWithinWindowReplace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ActiveLiveness = 10 * time.Millisecond
		c.Cycle.ProbeWindow = 200 * time.Millisecond
		c.Cycle.ProbeBudgetPerMin = 30
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old-active"), 31000)
	setReady(m, testReady("probe-candidate", 9))
	fr.setProbeFn(func(ctx context.Context, addr string) error { return errTest })

	m.probeOnce(context.Background())
	m.probeOnce(context.Background())
	time.Sleep(20 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("two failed probes triggered %d promotions, want 0", got)
	}
	m.probeOnce(context.Background())
	eventually(t, 2*time.Second, "liveness replacement", func() bool {
		return activeHash(m) == "probe-candidate"
	})
}

func TestProbeSuccessResetsFailureWindow(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ActiveLiveness = 10 * time.Millisecond
		c.Cycle.ProbeWindow = 200 * time.Millisecond
		c.Cycle.ProbeBudgetPerMin = 30
	})
	setActive(t, m, testLink("old-active"), 31001)
	setReady(m, testReady("probe-candidate", 9))
	results := []error{errTest, errTest, nil, errTest}
	i := 0
	fr.setProbeFn(func(ctx context.Context, addr string) error {
		err := results[i]
		i++
		return err
	})
	for range results {
		m.probeOnce(context.Background())
	}
	time.Sleep(80 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("successful probe did not reset failures; promotions = %d", got)
	}
	if activeHash(m) != "old-active" {
		t.Fatalf("active = %q, want unchanged", activeHash(m))
	}
}

func TestProbeFailureReplacementCooldown(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ActiveLiveness = 10 * time.Millisecond
		c.Cycle.ProbeBudgetPerMin = 30
		c.Cycle.ProbeCount = 1
	})
	setActive(t, m, testLink("old-active"), 31003)
	setReady(m, testReady("probe-candidate", 9))
	fr.setProbeFn(func(ctx context.Context, addr string) error { return errTest })
	m.mu.Lock()
	m.nextReplaceAt = time.Now().Add(time.Hour)
	m.mu.Unlock()

	m.probeOnce(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := len(fr.promotionLinks()); got != 0 {
		t.Fatalf("cooldown did not suppress replacement; promotions = %d", got)
	}
	m.mu.Lock()
	failures := m.probeFails
	m.mu.Unlock()
	if failures != 0 {
		t.Fatalf("probeFails = %d after cooldown, want 0", failures)
	}
}

func TestProbeBudgetIsGlobal(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ActiveLiveness = 10 * time.Millisecond
		c.Cycle.ProbeBudgetPerMin = 2
	})
	setActive(t, m, testLink("active"), 31002)
	for i := 0; i < 10; i++ {
		m.probeOnce(context.Background())
	}
	_, _, probes := fr.counts()
	if probes != 2 {
		t.Fatalf("probes = %d, want 2 (budget)", probes)
	}
	m.mu.Lock()
	tokens := m.probeTokens
	m.mu.Unlock()
	if tokens >= 1 {
		t.Fatalf("tokens = %v, want < 1 after budget exhausted", tokens)
	}
}

func TestProbeSkipsWithoutActive(t *testing.T) {
	m, fr, _ := testManager(t, nil)
	m.probeOnce(context.Background())
	_, _, probes := fr.counts()
	if probes != 0 {
		t.Fatalf("probes = %d without active, want 0", probes)
	}
}
