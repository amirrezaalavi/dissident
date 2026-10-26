package wan

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func TestCycleRunsTestsConcurrently(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Test.CheapWorkers = 4
		c.Test.RealWorkers = 2
		c.Cycle.ReadyTarget = 3
		c.Cycle.ReadyMin = 1
	})
	links := make([]contract.Link, 0, 8)
	for i := 0; i < 8; i++ {
		links = append(links, testLink("concurrent-"+string(rune('a'+i))))
	}
	store.Merge(links)

	var active, maximum int32
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		current := atomic.AddInt32(&active, 1)
		for {
			previous := atomic.LoadInt32(&maximum)
			if current <= previous || atomic.CompareAndSwapInt32(&maximum, previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return testResult(link, 5)
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := atomic.LoadInt32(&maximum); got < 2 {
		t.Fatalf("cycle test concurrency = %d, want at least 2", got)
	}
	if tests, _, _ := fr.counts(); tests != len(links) {
		t.Fatalf("tests = %d, want %d", tests, len(links))
	}
	if got := len(readyHashes(m)); got != 3 {
		t.Fatalf("ready = %d, want 3", got)
	}
}

func TestCyclePromotesReadyWhenNoActive(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	setReady(m, testReady("restored-standby", 7))

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got != "restored-standby" {
		t.Fatalf("active = %q, want restored-standby", got)
	}
	if got := len(fr.promotionLinks()); got != 1 {
		t.Fatalf("promotions = %d, want 1", got)
	}
}

func TestCycleSkipsWhenAlreadyAtTarget(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, testLink("current"), 29200)
	setReady(m, testReady("ready-one", 3), testReady("ready-two", 2))
	store.Merge([]contract.Link{testLink("pending")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if tests, _, _ := fr.counts(); tests != 0 {
		t.Fatalf("tests = %d, want 0 when active + target are already present", tests)
	}
}

func TestCycleSkipsFailedEntryDuringCooldown(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.FailedRetry = time.Hour
	})
	link := testLink("cooling")
	store.Merge([]contract.Link{link})
	store.Mark(link.Hash, contract.StateFailed, contract.Result{
		Hash: link.Hash, Stage: contract.StageTCP, OK: false,
		Reason: "recent failure", At: time.Now().UTC(),
	})

	if err := m.RunCycle(context.Background()); err == nil {
		t.Fatal("RunCycle without active WAN should return an error")
	}
	if tests, _, _ := fr.counts(); tests != 0 {
		t.Fatalf("tests = %d, want 0 while failed entry is cooling down", tests)
	}
}
