package wan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func TestCycleFillsReadyAndPromotesFirstPasser(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	store.Merge([]contract.Link{testLink("one"), testLink("two"), testLink("three")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got == "" {
		t.Fatal("cycle did not promote an active link")
	}
	hashes := readyHashes(m)
	if len(hashes) != 2 {
		t.Fatalf("ready = %v, want 2 entries", hashes)
	}
	if contains(hashes, activeHash(m)) {
		t.Fatalf("active %q also present in ready %v", activeHash(m), hashes)
	}
	tests, promotes, _ := fr.counts()
	if tests != 3 {
		t.Fatalf("tests = %d, want 3 (active + 2 ready)", tests)
	}
	if promotes != 1 {
		t.Fatalf("promotions = %d, want 1", promotes)
	}
	if m.LastCycle().IsZero() || m.NextCycle().IsZero() || !m.NextCycle().After(m.LastCycle()) {
		t.Fatalf("cycle timestamps not recorded: last=%v next=%v", m.LastCycle(), m.NextCycle())
	}
	if m.Cycling() {
		t.Fatal("Cycling stays true after RunCycle")
	}
}

func TestCyclePromotionFallsBackToVerificationOnly(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	store.Merge([]contract.Link{testLink("flaky-cycle")})
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("speed endpoint timed out")
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash != "flaky-cycle" {
			t.Fatalf("verification fallback got %q", link.Hash)
		}
		verified++
		return &fakeProcess{port: 29100, pid: 7900}, nil
	}

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := activeHash(m); got != "flaky-cycle" {
		t.Fatalf("active = %q, want fallback-promoted candidate", got)
	}
	if verified != 1 {
		t.Fatalf("verification fallback calls = %d, want 1", verified)
	}
	if tests, _, _ := fr.counts(); tests != 1 {
		t.Fatalf("tests = %d, want the candidate tested once", tests)
	}
}

func TestCycleSkipsActiveAndReady(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	setActive(t, m, testLink("active"), 29000)
	setReady(m, testReady("standby", 2))
	store.Merge([]contract.Link{testLink("active"), testLink("standby"), testLink("new")})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	calls := fr.testLinks()
	if len(calls) != 1 || calls[0] != "new" {
		t.Fatalf("test calls = %v, want [new]", calls)
	}
	if len(readyHashes(m)) != 2 {
		t.Fatalf("ready = %v, want active skip + new", readyHashes(m))
	}
}

func TestCycleFetchFailureKeepsLastKnownGood(t *testing.T) {
	m, _, _ := testManager(t, nil)
	setActive(t, m, testLink("active"), 29001)
	setReady(m, testReady("standby", 2))
	routeBefore, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route before: %v", err)
	}
	if err := os.Remove(m.currentConfig().Paths.URLsFile); err != nil {
		t.Fatalf("remove urls file: %v", err)
	}
	if err := m.RunCycle(context.Background()); err == nil {
		t.Fatal("RunCycle with missing urls_file should report the fetch error")
	}
	if got := activeHash(m); got != "active" {
		t.Fatalf("active = %q, want last-known-good", got)
	}
	if hashes := readyHashes(m); len(hashes) != 1 || hashes[0] != "standby" {
		t.Fatalf("ready = %v, want unchanged", hashes)
	}
	routeAfter, err := m.Route(context.Background())
	if err != nil {
		t.Fatalf("Route after: %v", err)
	}
	if routeAfter.ID != routeBefore.ID {
		t.Fatalf("route changed on fetch failure: %q -> %q", routeBefore.ID, routeAfter.ID)
	}
	if m.LastError() == "" {
		t.Fatal("LastError is empty after fetch failure")
	}
}

func TestCycleTestsPendingEvenWhenFetchFails(t *testing.T) {
	m, _, store := testManager(t, nil)
	store.Merge([]contract.Link{testLink("pending")})
	if err := os.Remove(m.currentConfig().Paths.URLsFile); err != nil {
		t.Fatalf("remove urls file: %v", err)
	}
	_ = m.RunCycle(context.Background())
	if got := activeHash(m); got != "pending" {
		t.Fatalf("active = %q, want pending link promoted despite fetch failure", got)
	}
}

func TestCycleCancelStopsEarly(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 5
	})
	links := make([]contract.Link, 0, 10)
	for i := 0; i < 10; i++ {
		links = append(links, testLink("cancel-"+string(rune('a'+i))))
	}
	store.Merge(links)

	testStarted := make(chan struct{}, 1)
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		select {
		case testStarted <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return contract.Result{Hash: link.Hash, OK: false, Reason: "canceled", At: time.Now().UTC()}
	})

	errCh := make(chan error, 1)
	go func() { errCh <- m.RunCycle(context.Background()) }()
	select {
	case <-testStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cycle never started testing")
	}
	if !m.Cycling() {
		t.Fatal("Cycling is false while a cycle is running")
	}
	m.CancelCycle()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCycle error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunCycle ignored CancelCycle")
	}
	if m.Cycling() {
		t.Fatal("Cycling stayed true after cancel")
	}
	if m.LastCycle().IsZero() || m.NextCycle().IsZero() {
		t.Fatal("canceled cycle did not record timestamps")
	}
	_, _, _ = fr.counts()
	for _, entry := range store.Entries() {
		if entry.State == contract.StateTesting {
			t.Fatalf("canceled cycle left %q stuck in testing", entry.Link.Hash)
		}
	}
}

func TestRunCycleGuardsConcurrentCycles(t *testing.T) {
	m, fr, store := testManager(t, nil)
	store.Merge([]contract.Link{testLink("x")})
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return contract.Result{Hash: link.Hash, OK: true, At: time.Now().UTC(), SpeedMbps: 1}
	})
	errCh := make(chan error, 1)
	go func() { errCh <- m.RunCycle(context.Background()) }()
	<-started
	if err := m.RunCycle(context.Background()); !errors.Is(err, errAlreadyCycling) {
		t.Fatalf("second RunCycle = %v, want errAlreadyCycling", err)
	}
	close(release)
	if err := <-errCh; err != nil {
		t.Fatalf("first RunCycle: %v", err)
	}
}

func TestCycleCapsReadyTarget(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
	})
	setActive(t, m, testLink("active"), 29002)
	store.Merge([]contract.Link{testLink("a"), testLink("b"), testLink("c")})
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if got := len(readyHashes(m)); got != 2 {
		t.Fatalf("ready = %d, want target 2", got)
	}
}

func TestCycleSingleFailingLinkTerminatesOnce(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	store.Merge([]contract.Link{testLink("only-fail")})
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		return contract.Result{
			Hash: link.Hash, Stage: contract.StageTCP, OK: false,
			Reason: "unreachable", At: time.Now().UTC(),
		}
	})

	done := make(chan error, 1)
	go func() { done <- m.RunCycle(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cycle with only a failing link should report no active WAN")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunCycle spun forever on the single failing link")
	}
	if tests, _, _ := fr.counts(); tests != 1 {
		t.Fatalf("tests = %d, want exactly 1 attempt for the failing link", tests)
	}
	if m.Cycling() {
		t.Fatal("Cycling stayed true after the failing cycle returned")
	}
	if m.LastError() == "" {
		t.Fatal("LastError empty after a failed cycle")
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "only-fail" && entry.State != contract.StateFailed {
			t.Fatalf("entry state = %q, want failed", entry.State)
		}
	}
}

func TestCycleAllFailuresTerminates(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 3
	})
	store.Merge([]contract.Link{testLink("fail-a"), testLink("fail-b"), testLink("fail-c")})
	fr.setTestFn(func(ctx context.Context, link contract.Link) contract.Result {
		return contract.Result{
			Hash: link.Hash, Stage: contract.StageTCP, OK: false,
			Reason: "unreachable", At: time.Now().UTC(),
		}
	})

	done := make(chan error, 1)
	go func() { done <- m.RunCycle(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("all-failing cycle should return an error (no active WAN)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("all-failing cycle did not terminate; failed entries were retried")
	}
	if tests, _, _ := fr.counts(); tests != 3 {
		t.Fatalf("tests = %d, want each pending entry tested once", tests)
	}
}

func TestCycleNoUsableSourcesNoActiveIsError(t *testing.T) {
	m, _, _ := testManager(t, nil)
	if err := m.RunCycle(context.Background()); err == nil {
		t.Fatal("cycle with no sources and no active should return an error")
	}
	if m.LastError() == "" {
		t.Fatal("LastError empty")
	}
}

func TestSubscriptionsFileWithShareLinkStillLoads(t *testing.T) {
	m, _, _ := testManager(t, nil)
	dir := filepath.Dir(m.statePath())
	path := filepath.Join(dir, "urls.txt")
	line := testLink("share").Raw + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle with a local share link: %v", err)
	}
	if got := activeHash(m); got == "" {
		t.Fatal("parsed share link was not promoted")
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
