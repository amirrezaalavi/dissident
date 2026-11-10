package wan

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/config"
	"dissident/internal/contract"
)

// TestScenarioDeadProcessIsDetectedAndReplaced covers a hard state change:
// the active xray child exits while standbys are available. The manager must
// stop routing to the dead loopback port and promote a standby on the next
// liveness tick, without waiting for three failed probes.
func TestScenarioDeadProcessIsDetectedAndReplaced(t *testing.T) {
	m, _, _ := testManager(t, nil)
	old := setActive(t, m, testLink("old"), 30001)
	setReady(m, testReady("spare", 1))

	_ = old.Stop(context.Background())
	if m.hasActive() {
		t.Fatal("hasActive() stayed true after the xray child exited")
	}
	if _, err := m.Route(context.Background()); !errors.Is(err, errNoActive) {
		t.Fatalf("Route on a dead child = %v, want errNoActive", err)
	}

	m.probeOnce(context.Background())
	eventually(t, 2*time.Second, "dead active replaced by standby", func() bool {
		return activeHash(m) == "spare"
	})
}

// TestScenarioCyclePromotesAfterProcessDeath covers recovery driven by the
// cycle rather than liveness: the active child is gone and a fresh candidate
// passes. The cycle must install it directly instead of only adding it to the
// ready list and leaving the proxy dead.
func TestScenarioCyclePromotesAfterProcessDeath(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 1
		c.Cycle.ReadyMin = 1
	})
	old := setActive(t, m, testLink("old"), 30002)
	_ = old.Stop(context.Background())
	store.Merge([]contract.Link{testLink("fresh")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got != "fresh" {
		t.Fatalf("active = %q, want fresh promoted by the cycle", got)
	}
}

// TestScenarioSteadyStateSkipsCycle covers a configuration already at target:
// no tests may run and the active route must not change.
func TestScenarioSteadyStateSkipsCycle(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, testLink("active"), 30003)
	setReady(m, testReady("a", 2), testReady("b", 1))

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if tests, _, _ := fr.counts(); tests != 0 {
		t.Fatalf("tests = %d, want 0 when already at target", tests)
	}
	if got := activeHash(m); got != "active" {
		t.Fatalf("active = %q, want unchanged", got)
	}
}

// TestScenarioCancelRequeuesInFlightEntries covers operator cancellation and
// shutdown. An in-flight candidate was never evaluated, so cancellation must
// not put it in the failed-retry cooldown.
func TestScenarioCancelRequeuesInFlightEntries(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 5
		c.Cycle.ReadyMin = 5
	})
	setActive(t, m, testLink("active"), 30004)
	links := make([]contract.Link, 0, 40)
	for i := 0; i < 40; i++ {
		links = append(links, testLink("cancel-"+strconv.Itoa(i)))
	}
	store.Merge(links)

	started := make(chan struct{}, 1)
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return contract.Result{Hash: link.Hash, Stage: contract.StageReal, OK: false, Reason: "canceled", At: time.Now().UTC()}
	})

	done := make(chan error, 1)
	go func() { done <- m.RunCycle(context.Background()) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle never launched a test")
	}
	m.CancelCycle()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCycle did not return after cancel")
	}

	for _, entry := range store.Entries() {
		if entry.State == contract.StateFailed && strings.Contains(entry.Last.Reason, "cycle canceled") {
			t.Fatalf("canceled entry %s was marked failed: %+v", entry.Link.Hash, entry.Last)
		}
	}
}

// TestScenarioTransientPromotionFailureKeepsPool covers a short upstream
// outage: every promotion fails once. The pool must survive that, then evict a
// candidate only after repeated failures.
func TestScenarioTransientPromotionFailureKeepsPool(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Swap.PromotionTimeout = 50 * time.Millisecond
	})
	setActive(t, m, testLink("old"), 30005)
	setReady(m, testReady("a", 3), testReady("b", 2))
	store.Merge([]contract.Link{testLink("a"), testLink("b")})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("EOF")
	})
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("EOF")
	}
	m.cycling.Store(true)
	defer m.cycling.Store(false)

	_ = m.Replace(context.Background())
	if hashes := readyHashes(m); len(hashes) != 2 {
		t.Fatalf("after one failed pass ready = %v, want both standbys kept", hashes)
	}
	for i := 1; i < promotionFailureThreshold; i++ {
		_ = m.Replace(context.Background())
	}
	if hashes := readyHashes(m); len(hashes) != 0 {
		t.Fatalf("ready = %v, want both evicted after %d failures", hashes, promotionFailureThreshold)
	}
}

// TestScenarioTunnelDeadProcessAlive covers the hard case: xray is alive but
// the tunnel is dead. A cycle can find a good candidate, but with the old
// child still running it can only queue it; failover waits for the liveness
// threshold and replacement cooldown. This test documents that latency.
func TestScenarioTunnelDeadProcessAlive(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
	})
	setActive(t, m, testLink("stuck"), 30006)
	store.Merge([]contract.Link{testLink("fresh")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got != "stuck" {
		t.Fatalf("active = %q, want the live-but-stuck child left alone", got)
	}
	if hashes := readyHashes(m); len(hashes) != 1 || hashes[0] != "fresh" {
		t.Fatalf("ready = %v, want fresh queued for later promotion", hashes)
	}
}

// TestScenarioDNSFailureDoesNotBlacklist covers a local resolver outage during
// a cycle. Candidates must stay eligible instead of entering the 30-minute
// failed-retry cooldown for a failure that was never theirs.
func TestScenarioDNSFailureDoesNotBlacklist(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 5
		c.Cycle.ReadyMin = 5
	})
	setActive(t, m, testLink("active"), 30007)
	links := make([]contract.Link, 0, 4)
	for i := 0; i < 4; i++ {
		links = append(links, testLink("dns-"+strconv.Itoa(i)))
	}
	store.Merge(links)
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		return contract.Result{
			Hash: link.Hash, Stage: contract.StageTCP, OK: false,
			Transient: true, Reason: "tcp: lookup example.com: i/o timeout",
			At: time.Now().UTC(),
		}
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "active" {
			continue
		}
		if entry.State != contract.StateCatalog {
			t.Fatalf("DNS-failed entry %s state = %q, want catalog", entry.Link.Hash, entry.State)
		}
	}
}

// TestScenarioHardTCPFailureStillBlacklists ensures the transient exception is
// narrow: a plain TCP failure keeps the failed-retry cooldown.
func TestScenarioHardTCPFailureStillBlacklists(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 5
		c.Cycle.ReadyMin = 5
	})
	setActive(t, m, testLink("active"), 30008)
	store.Merge([]contract.Link{testLink("refused")})
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		return contract.Result{
			Hash: link.Hash, Stage: contract.StageTCP, OK: false,
			Reason: "tcp: connection refused", At: time.Now().UTC(),
		}
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "refused" && entry.State != contract.StateFailed {
			t.Fatalf("hard TCP failure state = %q, want failed", entry.State)
		}
	}
}

// TestScenarioOperatorDropSurvivesRestart covers the panel's Drop action end
// to end: a ready standby is dropped, the pool persists the exclusion, a
// restart restores the exclusion instead of re-merging the link, and a later
// cycle cannot quietly requalify it.
func TestScenarioOperatorDropSurvivesRestart(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, testLink("live"), 30300)
	dropped := testLink("dropped")
	store.Merge([]contract.Link{dropped, testLink("keep")})
	setReady(m, testReady("dropped", 2), testReady("keep", 1))

	if err := m.RemoveReady("dropped"); err != nil {
		t.Fatalf("RemoveReady: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := m.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopCancel()

	// Restart against the same state file, with a fresh in-memory catalog that
	// contains the dropped link again (as a new process would after sources
	// are merged).
	store2 := catalog.New()
	store2.Merge([]contract.Link{dropped, testLink("keep")})
	m2 := New(Options{
		Catalog:   store2,
		Config:    m.config,
		StateFile: m.statePath(),
		Logger:    discardLogger(),
	})
	fr2 := &fakeRuntime{}
	fr2.install(m2)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m2.Stop(ctx)
	})
	if err := m2.Start(context.Background()); err != nil {
		t.Fatalf("restarted Start: %v", err)
	}

	if !m2.isExcluded("dropped") {
		t.Fatal("operator exclusion did not survive restart")
	}
	snap := m2.Snapshot()
	if len(snap.Excluded) != 1 || snap.Excluded[0].Hash != "dropped" {
		t.Fatalf("snapshot excluded = %+v", snap.Excluded)
	}
	for _, hash := range readyHashes(m2) {
		if hash == "dropped" {
			t.Fatal("excluded link returned to the ready pool after restart")
		}
	}
	if entry := entryByHash(t, store2, "dropped"); entry.State != contract.StateExcluded {
		t.Fatalf("catalog state after restart = %q, want excluded", entry.State)
	}

	// Even if a future merge ever resets the entry to catalog, the cycle queue
	// must still skip it.
	store2.Reclassify("dropped", contract.StateCatalog, "")
	if err := m2.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle after restart: %v", err)
	}
	for _, hash := range fr2.testLinks() {
		if hash == "dropped" {
			t.Fatal("cycle tested an operator-dropped link after restart")
		}
	}
}
