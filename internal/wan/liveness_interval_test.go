package wan

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dissident/internal/config"
)

func TestProbeFailuresAcrossLivenessIntervalsReplace(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ActiveLiveness = 15 * time.Second
		c.Cycle.ProbeWindow = 8 * time.Second
		c.Cycle.ProbeCount = 3
		c.Cycle.ProbeBudgetPerMin = 60
		c.Swap.DrainTimeout = 20 * time.Millisecond
	})
	setActive(t, m, testLink("old-active"), 31100)
	setReady(m, testReady("interval-candidate", 9))
	fr.setProbeFn(func(context.Context, string) error { return errTest })

	base := time.Now()
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	m.now = func() time.Time { return time.Unix(0, clock.Load()) }

	// Probes run one per active_liveness interval. With a fixed 8s window the
	// three failures would never overlap; the effective window must cover the
	// configured probe schedule.
	for i := 0; i < 3; i++ {
		m.probeOnce(context.Background())
		clock.Add(int64(15 * time.Second))
	}

	eventually(t, 2*time.Second, "liveness replacement across intervals", func() bool {
		return activeHash(m) == "interval-candidate"
	})
}
