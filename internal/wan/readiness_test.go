package wan

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
)

func readyFor(hash, server string, port int, score float64) contract.Ready {
	link := testLink(hash)
	link.Server = server
	link.Port = port
	return contract.Ready{
		Link:      link,
		Score:     score,
		LastCheck: time.Now().Add(-time.Hour).UTC(),
		ExitIP:    "203.0.113.9",
	}
}

func pipeConn() net.Conn {
	client, peer := net.Pipe()
	peer.Close()
	return client
}

func TestReadyLivenessDropsFailedAndKeepsGood(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 3
		c.Cycle.ReadyLiveness = time.Hour
	})
	bad := readyFor("ready-bad", "bad.example", 443, 5)
	good := readyFor("ready-good", "good.example", 8443, 4)
	badAddr := net.JoinHostPort(bad.Link.Server, strconv.Itoa(bad.Link.Port))
	goodAddr := net.JoinHostPort(good.Link.Server, strconv.Itoa(good.Link.Port))
	setReady(m, bad, good)
	store.Merge([]contract.Link{bad.Link, good.Link})
	oldCheck := good.LastCheck

	var mu sync.Mutex
	var dialed []string
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, address)
		mu.Unlock()
		switch address {
		case badAddr:
			return nil, errTest
		case goodAddr:
			return pipeConn(), nil
		default:
			return nil, errors.New("unexpected address")
		}
	}

	m.checkReadyOnce(context.Background())

	if hashes := readyHashes(m); len(hashes) != 1 || hashes[0] != "ready-good" {
		t.Fatalf("ready = %v, want only ready-good", hashes)
	}
	m.mu.Lock()
	gotCheck := m.ready[0].LastCheck
	m.mu.Unlock()
	if !gotCheck.After(oldCheck) {
		t.Fatalf("good LastCheck = %v, want after %v", gotCheck, oldCheck)
	}
	for _, entry := range store.Entries() {
		if entry.Link.Hash == "ready-bad" && entry.State != contract.StateFailed {
			t.Fatalf("bad entry state = %q, want failed", entry.State)
		}
	}
	if tests, _, _ := fr.counts(); tests != 0 {
		t.Fatalf("ready liveness called full Test %d times, want 0", tests)
	}
	mu.Lock()
	gotDialed := append([]string(nil), dialed...)
	mu.Unlock()
	if len(gotDialed) != 2 {
		t.Fatalf("dialed = %v, want two TCP checks", gotDialed)
	}

	data, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var saved persistedState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Ready) != 1 || saved.Ready[0].Link.Hash != "ready-good" {
		t.Fatalf("persisted ready = %+v, want only ready-good", saved.Ready)
	}
}

func TestReadyLivenessLowCountTriggersRefill(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 2
		c.Cycle.ReadyTarget = 3
	})
	setActive(t, m, testLink("active"), 32000)
	good := readyFor("ready-good", "good.example", 443, 4)
	setReady(m, good)
	store.Merge([]contract.Link{testLink("pending-refill")})
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		return pipeConn(), nil
	}

	m.checkReadyOnce(context.Background())
	eventually(t, 2*time.Second, "low ready count to trigger a refill cycle", func() bool {
		for _, hash := range fr.testLinks() {
			if hash == "pending-refill" {
				return true
			}
		}
		return false
	})
}

func TestRemoveReadyBelowMinimumTriggersRefill(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 2
		c.Cycle.ReadyTarget = 3
	})
	setActive(t, m, testLink("active"), 32001)
	setReady(m, readyFor("only-ready", "ready.example", 443, 4))
	store.Merge([]contract.Link{testLink("pending-after-remove")})

	if err := m.RemoveReady("only-ready"); err != nil {
		t.Fatalf("RemoveReady: %v", err)
	}
	eventually(t, 2*time.Second, "RemoveReady to trigger a refill cycle", func() bool {
		for _, hash := range fr.testLinks() {
			if hash == "pending-after-remove" {
				return true
			}
		}
		return false
	})
}

func TestReadyLivenessUsesConfiguredEgressDialerOnly(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Test.EgressProxy = "http://127.0.0.1:3128"
	})
	entry := readyFor("egress-ready", "egress.example", 443, 4)
	setReady(m, entry)
	store.Merge([]contract.Link{entry.Link})

	var mu sync.Mutex
	directCalls, egressBuilds, egressCalls := 0, 0, 0
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		mu.Lock()
		directCalls++
		mu.Unlock()
		return pipeConn(), nil
	}
	m.egressDial = func(proxy string, timeout time.Duration) (func(context.Context, string, string) (net.Conn, error), error) {
		mu.Lock()
		egressBuilds++
		mu.Unlock()
		if proxy != "http://127.0.0.1:3128" {
			return nil, errors.New("unexpected proxy")
		}
		return func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			egressCalls++
			mu.Unlock()
			return nil, errTest
		}, nil
	}

	m.checkReadyOnce(context.Background())

	mu.Lock()
	gotDirect, gotBuilds, gotEgress := directCalls, egressBuilds, egressCalls
	mu.Unlock()
	if gotDirect != 0 {
		t.Fatalf("direct dials = %d, want 0 when egress is configured", gotDirect)
	}
	if gotBuilds != 1 || gotEgress != 1 {
		t.Fatalf("egress builds/calls = %d/%d, want 1/1", gotBuilds, gotEgress)
	}
	if hashes := readyHashes(m); len(hashes) != 0 {
		t.Fatalf("ready = %v, want failed egress entry removed", hashes)
	}
}

func TestReadyLivenessCancellationDoesNotDropReady(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
	})
	setReady(m,
		readyFor("ready-a", "a.example", 443, 2),
		readyFor("ready-b", "b.example", 443, 1),
	)
	ctx, cancel := context.WithCancel(context.Background())
	m.dial = func(dctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		cancel()
		<-dctx.Done()
		return nil, dctx.Err()
	}

	m.checkReadyOnce(ctx)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("test context was not canceled")
	}
	if hashes := readyHashes(m); len(hashes) != 2 {
		t.Fatalf("ready = %v, cancellation must not remove standbys", hashes)
	}
}

func TestReadyLivenessLoopStartsAndStops(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyLiveness = 15 * time.Millisecond
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 3
	})
	setActive(t, m, testLink("loop-active"), 33000)
	setReady(m, readyFor("loop-ready", "loop.example", 443, 4))
	if err := m.saveState(); err != nil {
		t.Fatalf("save state: %v", err)
	}
	var mu sync.Mutex
	dials := 0
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return nil, errTest
	}

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, 2*time.Second, "ready liveness loop to drop the failed standby", func() bool {
		return len(readyHashes(m)) == 0
	})
	mu.Lock()
	beforeStop := dials
	mu.Unlock()
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	afterStop := dials
	mu.Unlock()
	if afterStop != beforeStop {
		t.Fatalf("ready liveness kept dialing after Stop: %d -> %d", beforeStop, afterStop)
	}
}

func TestStartFallsBackToVerificationOnlyRestore(t *testing.T) {
	m, fr, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 3
	})
	active := testLink("persisted-active")
	state := persistedState{
		Version: stateVersion,
		Active: &contract.Active{
			Link: active, Since: time.Now().Add(-time.Hour).UTC(), ExitIP: "203.0.113.88",
		},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.statePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	fr.setPromoteFn(func(ctx context.Context, link contract.Link) (process, error) {
		return nil, errors.New("speed endpoint unavailable")
	})
	verified := 0
	m.startVerified = func(ctx context.Context, link contract.Link) (process, error) {
		if link.Hash != "persisted-active" {
			t.Fatalf("fallback got link %q", link.Hash)
		}
		verified++
		return &fakeProcess{port: 26010, pid: 9100}, nil
	}

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := activeHash(m); got != "persisted-active" {
		t.Fatalf("active = %q, want persisted-active restored by fallback", got)
	}
	if verified != 1 {
		t.Fatalf("verification fallback calls = %d, want 1", verified)
	}
	if calls := fr.promotionLinks(); len(calls) != 1 || calls[0] != "persisted-active" {
		t.Fatalf("promote calls = %v, want one failed full Promote", calls)
	}
	snap := m.Snapshot()
	if snap.Active == nil || snap.Active.LocalPort != 26010 || snap.Active.XrayPID != 9100 {
		t.Fatalf("snapshot active = %+v, want fallback process", snap.Active)
	}
	if _, err := m.Route(context.Background()); err != nil {
		t.Fatalf("Route after fallback restore: %v", err)
	}
}
