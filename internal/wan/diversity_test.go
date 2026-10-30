package wan

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/provider"
)

func linkAt(hash, server string, port int) contract.Link {
	link := testLink(hash)
	link.Server = server
	link.Port = port
	link.Raw = fmt.Sprintf("vless://%s@%s:%d?type=tcp&security=none#%s",
		testUUID, server, port, hash)
	return link
}

func entryAt(hash, server string, port int) contract.Entry {
	return contract.Entry{Link: linkAt(hash, server, port), State: contract.StateCatalog}
}

func readyAt(hash, server string, port int, score float64) contract.Ready {
	return contract.Ready{
		Link:      linkAt(hash, server, port),
		Score:     score,
		LastCheck: time.Now().Add(-time.Minute).UTC(),
		ExitIP:    "203.0.113.9",
	}
}

func hasHash(hashes []string, want string) bool {
	for _, h := range hashes {
		if h == want {
			return true
		}
	}
	return false
}

func TestBuildCycleQueueReservesNonCloudflareFirst(t *testing.T) {
	entries := []contract.Entry{
		entryAt("cf-a", "104.16.0.1", 443),
		entryAt("non-a", "8.8.8.8", 443),
		entryAt("cf-b", "104.16.0.2", 443),
		entryAt("non-b", "9.9.9.9", 443),
	}
	queue, queued := buildCycleQueue(entries, true, false, diversityReserveBatch, 0)
	if queued != 2 {
		t.Fatalf("reserveQueued = %d, want 2", queued)
	}
	if len(queue) != 4 {
		t.Fatalf("queue = %d entries, want 4", len(queue))
	}
	if queue[0].Link.Hash != "non-a" || queue[1].Link.Hash != "non-b" {
		t.Fatalf("queue prefix = %q,%q, want non-Cloudflare first", queue[0].Link.Hash, queue[1].Link.Hash)
	}
	for _, entry := range queue[2:] {
		if provider.ClassifyLink(entry.Link) != provider.Cloudflare {
			t.Fatalf("tail entry %q is not Cloudflare", entry.Link.Hash)
		}
	}

	plain, plainQueued := buildCycleQueue(entries, false, false, diversityReserveBatch, 2)
	if plainQueued != 0 {
		t.Fatalf("plain reserveQueued = %d, want 0", plainQueued)
	}
	if len(plain) != 2 || plain[0].Link.Hash != "cf-a" || plain[1].Link.Hash != "non-a" {
		t.Fatalf("plain queue = %v, want catalog order capped at 2", plain)
	}
}

func TestBuildCycleQueuePrefersDistinctEndpointAndPrefix(t *testing.T) {
	entries := []contract.Entry{
		entryAt("dup-endpoint", "8.8.8.8", 443),
		entryAt("same-endpoint", "8.8.8.8", 443),
		entryAt("distinct", "9.9.9.9", 443),
		entryAt("same-prefix", "8.8.8.9", 443),
	}
	queue, _ := buildCycleQueue(entries, true, false, diversityReserveBatch, 0)
	if len(queue) < 2 {
		t.Fatalf("queue = %v", queue)
	}
	if queue[0].Link.Hash != "dup-endpoint" || queue[1].Link.Hash != "distinct" {
		t.Fatalf("first tier = %q,%q, want distinct endpoint/prefix first",
			queue[0].Link.Hash, queue[1].Link.Hash)
	}
	if queue[2].Link.Hash != "same-endpoint" || queue[3].Link.Hash != "same-prefix" {
		t.Fatalf("backfill order = %q,%q, want catalog order after strict tier",
			queue[2].Link.Hash, queue[3].Link.Hash)
	}
}

func TestBuildCycleQueueBoundsNonCloudflareBatch(t *testing.T) {
	entries := make([]contract.Entry, 0, 134)
	for i := 0; i < 130; i++ {
		entries = append(entries, entryAt(
			fmt.Sprintf("non-%03d", i),
			fmt.Sprintf("8.%d.%d.%d", i/256, i%256, (i*7)%256),
			443,
		))
	}
	for i := 0; i < 4; i++ {
		entries = append(entries, entryAt(fmt.Sprintf("cf-%d", i), fmt.Sprintf("104.16.0.%d", i+1), 443))
	}
	queue, queued := buildCycleQueue(entries, true, false, diversityReserveBatch, 0)
	if queued != diversityReserveBatch {
		t.Fatalf("reserveQueued = %d, want %d", queued, diversityReserveBatch)
	}
	if len(queue) != diversityReserveBatch+4 {
		t.Fatalf("queue = %d entries, want %d (bounded non-CF + CF fill)", len(queue), diversityReserveBatch+4)
	}
	for i := 0; i < diversityReserveBatch; i++ {
		if provider.ClassifyLink(queue[i].Link) == provider.Cloudflare {
			t.Fatalf("queue[%d] is Cloudflare; non-Cloudflare batch was not first/bounded", i)
		}
	}
	for i := diversityReserveBatch; i < len(queue); i++ {
		if provider.ClassifyLink(queue[i].Link) != provider.Cloudflare {
			t.Fatalf("queue[%d] is not Cloudflare fill", i)
		}
	}

	limited, limitedQueued := buildCycleQueue(entries, true, false, diversityReserveBatch, 10)
	if len(limited) != 10 || limitedQueued != 10 {
		t.Fatalf("test cap queue = %d/%d, want 10/10", len(limited), limitedQueued)
	}
	reserveOnly, reserveOnlyQueued := buildCycleQueue(entries, true, true, diversityReserveBatch, 0)
	if len(reserveOnly) != diversityReserveBatch || reserveOnlyQueued != diversityReserveBatch {
		t.Fatalf("reserve-only queue = %d/%d, want %d", len(reserveOnly), reserveOnlyQueued, diversityReserveBatch)
	}
	for _, entry := range reserveOnly {
		if provider.ClassifyLink(entry.Link) != provider.NonCloudflare {
			t.Fatalf("reserve-only queue contains %q", entry.Link.Hash)
		}
	}
}

func TestCycleReserveFillsNonCloudflareWhenAvailable(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, linkAt("active-cf", "172.64.150.28", 2082), 34000)
	store.Merge([]contract.Link{
		linkAt("noncf", "8.8.8.8", 443),
		linkAt("cf-1", "104.16.0.1", 443),
		linkAt("cf-2", "104.16.0.2", 443),
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	hashes := readyHashes(m)
	if len(hashes) != 2 {
		t.Fatalf("ready = %v, want a full target", hashes)
	}
	if !hasHash(hashes, "noncf") {
		t.Fatalf("ready = %v, want the non-Cloudflare candidate admitted", hashes)
	}
	snap := m.Snapshot()
	if snap.Diversity.ReadyNonCloudflare < 1 {
		t.Fatalf("diversity = %+v, want a non-Cloudflare standby", snap.Diversity)
	}
	if snap.Diversity.Shortfall {
		t.Fatalf("shortfall recorded even though a non-Cloudflare standby qualified: %+v", snap.Diversity)
	}
}

func TestCycleOnlyCloudflareStillFillsAndRecordsShortfall(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, linkAt("active-cf", "172.64.150.28", 2082), 34001)
	store.Merge([]contract.Link{
		linkAt("cf-1", "104.16.0.1", 443),
		linkAt("cf-2", "104.17.0.1", 443),
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	hashes := readyHashes(m)
	if len(hashes) != 2 {
		t.Fatalf("ready = %v, want CF-only readiness to fill", hashes)
	}
	snap := m.Snapshot()
	if !snap.Diversity.SingleProvider || snap.Diversity.ReadyCloudflare != 2 {
		t.Fatalf("diversity = %+v, want a single-provider Cloudflare pool", snap.Diversity)
	}
	if !snap.Diversity.Shortfall || snap.Diversity.ShortfallEvents != 1 {
		t.Fatalf("diversity shortfall = %+v, want one event", snap.Diversity)
	}
	if snap.Diversity.LastShortfall.IsZero() {
		t.Fatal("shortfall timestamp is zero")
	}
}

func TestCycleReserveProbesWhenReadyTargetAlreadyMet(t *testing.T) {
	m, fr, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyTarget = 2
		c.Cycle.ReadyMin = 1
	})
	setActive(t, m, linkAt("active-cf", "172.64.150.28", 2082), 34002)
	setReady(m,
		readyAt("ready-cf-1", "104.16.0.5", 443, 9),
		readyAt("ready-cf-2", "104.16.0.6", 443, 8),
	)
	store.Merge([]contract.Link{
		linkAt("noncf", "8.8.8.8", 443),
		linkAt("cf-pending", "104.16.0.7", 443),
	})

	if err := m.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if calls := fr.testLinks(); len(calls) != 1 || calls[0] != "noncf" {
		t.Fatalf("test calls = %v, want only the reserve candidate", calls)
	}
	hashes := readyHashes(m)
	if len(hashes) != 2 || !hasHash(hashes, "noncf") {
		t.Fatalf("ready = %v, want the non-Cloudflare candidate to displace a duplicate", hashes)
	}
}

func TestReadyAdmissionCapsDuplicateEndpointAndPrefix(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 5
	})
	candidates := []contract.Ready{
		readyAt("a", "8.8.8.8", 443, 10),
		readyAt("b", "8.8.8.8", 443, 9),
		readyAt("c", "8.8.8.9", 443, 8),
		readyAt("d", "8.8.8.10", 443, 7),
		readyAt("e", "9.9.9.9", 443, 1),
	}
	for _, cand := range candidates {
		m.addReady(cand, 5)
	}
	hashes := readyHashes(m)
	if len(hashes) != 2 {
		t.Fatalf("ready = %v, want 2 entries after caps", hashes)
	}
	if hasHash(hashes, "b") || hasHash(hashes, "c") || hasHash(hashes, "d") {
		t.Fatalf("ready = %v, duplicate endpoint//24 entries were admitted", hashes)
	}
	perEndpoint := map[string]int{}
	perPrefix := map[string]int{}
	m.mu.Lock()
	for _, r := range m.ready {
		perEndpoint[provider.EndpointKey(r.Link)]++
		perPrefix[provider.PrefixKey(r.Link)]++
	}
	m.mu.Unlock()
	for ep, n := range perEndpoint {
		if n > maxReadyPerEndpoint {
			t.Fatalf("endpoint %s has %d ready entries, cap %d", ep, n, maxReadyPerEndpoint)
		}
	}
	for prefix, n := range perPrefix {
		if n > maxReadyPerPrefix {
			t.Fatalf("prefix %s has %d ready entries, cap %d", prefix, n, maxReadyPerPrefix)
		}
	}
}

func TestReadyAdmissionRelaxesCapsBelowMinimum(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 3
		c.Cycle.ReadyTarget = 3
	})
	candidates := []contract.Ready{
		readyAt("a", "8.8.8.8", 443, 40),
		readyAt("b", "8.8.8.8", 443, 30),
		readyAt("c", "8.8.8.8", 443, 20),
		readyAt("d", "8.8.8.8", 443, 10),
	}
	for _, cand := range candidates {
		m.addReady(cand, 3)
	}
	hashes := readyHashes(m)
	if len(hashes) != 3 {
		t.Fatalf("ready = %v, want 3 entries to satisfy ready_min", hashes)
	}
	if hasHash(hashes, "d") {
		t.Fatalf("ready = %v, the fourth duplicate should not displace the minimum set", hashes)
	}
}

func TestReadyAdmissionAvoidsActiveEndpoint(t *testing.T) {
	m, _, _ := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 2
	})
	setActive(t, m, linkAt("active", "8.8.8.8", 443), 34003)

	if admitted := m.addReady(readyAt("distinct", "9.9.9.9", 443, 1), 2); !admitted {
		t.Fatal("distinct candidate was not admitted")
	}
	if admitted := m.addReady(readyAt("same-as-active", "8.8.8.8", 443, 100), 2); admitted {
		t.Fatal("candidate duplicating the active endpoint was admitted")
	}
	if hashes := readyHashes(m); len(hashes) != 1 || hashes[0] != "distinct" {
		t.Fatalf("ready = %v, want only the distinct endpoint", hashes)
	}
}

func TestReadyLivenessRemovesDeadDiverseEntry(t *testing.T) {
	m, _, store := testManager(t, func(c *config.Config) {
		c.Cycle.ReadyMin = 1
		c.Cycle.ReadyTarget = 2
	})
	dead := readyAt("dead-noncf", "8.8.8.8", 443, 5)
	live := readyAt("live-cf", "104.16.0.9", 443, 4)
	setReady(m, dead, live)
	store.Merge([]contract.Link{dead.Link, live.Link})
	m.dial = func(ctx context.Context, network, address string, timeout time.Duration) (net.Conn, error) {
		if address == "8.8.8.8:443" {
			return nil, errTest
		}
		return pipeConn(), nil
	}

	m.checkReadyOnce(context.Background())
	hashes := readyHashes(m)
	if len(hashes) != 1 || hashes[0] != "live-cf" {
		t.Fatalf("ready = %v, a dead diverse entry must not be retained", hashes)
	}
}
