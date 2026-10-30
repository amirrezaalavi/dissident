package wan

import (
	"time"

	"dissident/internal/contract"
	"dissident/internal/provider"
)

const (
	// diversityReserveBatch bounds how many pending non-Cloudflare candidates a
	// cycle may test ahead of the normal Cloudflare fill. At the measured
	// non-Cloudflare full-pass rate of ~3.3%, 120 candidates give about a 98%
	// chance of at least one pass if candidates were independent. The real
	// catalog is clustered and dead targets dominate, so this is a planning
	// constant, not a guarantee. It also caps the extra work a diversity probe
	// can add when the pool cannot produce a non-Cloudflare standby.
	diversityReserveBatch = 120

	// maxReadyPerEndpoint is the soft cap on ready standbys sharing one exact
	// server:port. The cap is relaxed only while the pool would otherwise fall
	// below cycle.ready_min.
	maxReadyPerEndpoint = 1
	// maxReadyPerPrefix is the soft cap on ready standbys sharing one /24 (or
	// one hostname failure domain). A /24 is the failure-domain proxy used by
	// the catalog measurement. The cap is relaxed only while the pool would
	// otherwise fall below cycle.ready_min.
	maxReadyPerPrefix = 1
)

// buildCycleQueue builds this cycle's bounded qualification queue.
//
// Without a reserve request the input order is preserved (catalog order is
// already TCP-latency/oldest first). With a reserve request, at most
// nonCFLimit non-Cloudflare candidates are moved to the front; every other
// non-Cloudflare candidate is left for a later cycle, so the reserve cannot
// turn into an unbounded catalog scan. The remainder is filled in catalog order
// with Cloudflare and unknown entries. reserveOnly is used when the ready
// target is already met: the cycle probes diversity without adding more
// Cloudflare standbys.
//
// The returned reserveQueued count is the number of non-Cloudflare candidates
// actually placed in the queue; it is used for the shortfall event.
func buildCycleQueue(entries []contract.Entry, reserve, reserveOnly bool, nonCFLimit, testCap int) (queue []contract.Entry, reserveQueued int) {
	if !reserve {
		queue = append([]contract.Entry(nil), entries...)
		return applyTestCap(queue, testCap), 0
	}
	if nonCFLimit < 0 {
		nonCFLimit = 0
	}
	batch := selectReserveBatch(entries, nonCFLimit)
	if reserveOnly {
		batch = applyTestCap(batch, testCap)
		return batch, len(batch)
	}

	queue = make([]contract.Entry, 0, len(batch)+len(entries))
	queue = append(queue, batch...)
	used := make(map[string]struct{}, len(batch))
	for _, entry := range batch {
		used[entry.Link.Hash] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := used[entry.Link.Hash]; ok {
			continue
		}
		// Non-Cloudflare overflow is intentionally deferred to a later cycle:
		// the batch is a bound, not the start of a full-catalog sweep.
		if provider.ClassifyLink(entry.Link) == provider.NonCloudflare {
			continue
		}
		queue = append(queue, entry)
	}
	queue = applyTestCap(queue, testCap)
	return queue, countNonCloudflare(queue)
}

func countNonCloudflare(entries []contract.Entry) int {
	n := 0
	for _, entry := range entries {
		if provider.ClassifyLink(entry.Link) == provider.NonCloudflare {
			n++
		}
	}
	return n
}

func applyTestCap(queue []contract.Entry, testCap int) []contract.Entry {
	if testCap > 0 && len(queue) > testCap {
		return queue[:testCap]
	}
	return queue
}

// selectReserveBatch picks at most limit non-Cloudflare pending entries. It
// first takes entries with an unseen exact endpoint and an unseen failure
// domain, then backfills with the skipped entries. This keeps a batch of dead
// duplicates from consuming the whole reserve while preserving catalog order
// inside each tier.
func selectReserveBatch(entries []contract.Entry, limit int) []contract.Entry {
	if limit <= 0 {
		return nil
	}
	out := make([]contract.Entry, 0, limit)
	skipped := make([]contract.Entry, 0)
	seenEndpoint := make(map[string]struct{}, limit)
	seenPrefix := make(map[string]struct{}, limit)
	for _, entry := range entries {
		if provider.ClassifyLink(entry.Link) != provider.NonCloudflare {
			continue
		}
		endpoint := provider.EndpointKey(entry.Link)
		prefix := provider.PrefixKey(entry.Link)
		duplicateEndpoint := endpoint != ""
		if duplicateEndpoint {
			_, duplicateEndpoint = seenEndpoint[endpoint]
		}
		duplicatePrefix := prefix != ""
		if duplicatePrefix {
			_, duplicatePrefix = seenPrefix[prefix]
		}
		if duplicateEndpoint || duplicatePrefix {
			if len(skipped) < limit {
				skipped = append(skipped, entry)
			}
			continue
		}
		out = append(out, entry)
		if endpoint != "" {
			seenEndpoint[endpoint] = struct{}{}
		}
		if prefix != "" {
			seenPrefix[prefix] = struct{}{}
		}
		if len(out) >= limit {
			return out
		}
	}
	for _, entry := range skipped {
		if len(out) >= limit {
			break
		}
		out = append(out, entry)
	}
	return out
}

// hasNonCloudflare reports whether any ready entry is a known non-Cloudflare
// endpoint.
func hasNonCloudflare(ready []contract.Ready) bool {
	for _, r := range ready {
		if provider.ClassifyLink(r.Link) == provider.NonCloudflare {
			return true
		}
	}
	return false
}

// selectReadyWithCaps implements endpoint-aware ready admission.
//
// The pool is rebuilt from the existing standbys plus the candidate, ordered by
// score. A strict pass enforces the exact-endpoint and /24 caps, counting the
// active WAN as already occupying its endpoint and prefix so a standby cannot
// simply duplicate the serving route. If the strict pass cannot reach
// cycle.ready_min, a second pass fills from the remaining candidates while
// ignoring the caps: readiness is never starved for diversity.
//
// When the existing pool has no known non-Cloudflare entry, one non-Cloudflare
// candidate is forced in first. This is the soft reserve: it guarantees at
// least one diverse standby when one qualified, without requiring a second
// provider or turning the whole pool non-Cloudflare.
func selectReadyWithCaps(existing []contract.Ready, cand contract.Ready, target, minReady int, activeEndpoint, activePrefix string) []contract.Ready {
	if target < 1 {
		target = 1
	}
	if minReady < 1 {
		minReady = 1
	}
	if minReady > target {
		minReady = target
	}

	candidates := make([]contract.Ready, 0, len(existing)+1)
	candidates = append(candidates, existing...)
	candidates = append(candidates, cand)
	sortReady(candidates)

	selected := make([]contract.Ready, 0, target)
	chosen := make(map[string]struct{}, target)
	endpointCount := make(map[string]int, target)
	prefixCount := make(map[string]int, target)
	if activeEndpoint != "" {
		endpointCount[activeEndpoint] = 1
	}
	if activePrefix != "" {
		prefixCount[activePrefix] = 1
	}

	add := func(r contract.Ready) {
		selected = append(selected, r)
		chosen[r.Link.Hash] = struct{}{}
		if endpoint := provider.EndpointKey(r.Link); endpoint != "" {
			endpointCount[endpoint]++
		}
		if prefix := provider.PrefixKey(r.Link); prefix != "" {
			prefixCount[prefix]++
		}
	}

	// Keep one non-Cloudflare reserve slot filled. When the existing pool has
	// no non-Cloudflare entry, the candidate itself is forced if it qualifies
	// as one. Otherwise the best existing/candidate non-Cloudflare entry is
	// forced, so later Cloudflare churn cannot evict the reserve by score.
	// Prefer a forced entry that respects the endpoint/prefix caps; only relax
	// that preference when the pool is still below ready_min.
	strictOK := func(r contract.Ready) bool {
		endpoint := provider.EndpointKey(r.Link)
		prefix := provider.PrefixKey(r.Link)
		if endpoint != "" && endpointCount[endpoint] >= maxReadyPerEndpoint {
			return false
		}
		if prefix != "" && prefixCount[prefix] >= maxReadyPerPrefix {
			return false
		}
		return true
	}
	forceHash := ""
	for _, r := range candidates {
		if provider.ClassifyLink(r.Link) != provider.NonCloudflare {
			continue
		}
		if strictOK(r) {
			forceHash = r.Link.Hash
			break
		}
	}
	if forceHash == "" && len(selected) < minReady {
		for _, r := range candidates {
			if provider.ClassifyLink(r.Link) == provider.NonCloudflare {
				forceHash = r.Link.Hash
				break
			}
		}
	}
	if forceHash != "" {
		for _, r := range candidates {
			if r.Link.Hash == forceHash {
				add(r)
				break
			}
		}
	}

	for _, r := range candidates {
		if len(selected) >= target {
			break
		}
		if _, ok := chosen[r.Link.Hash]; ok {
			continue
		}
		endpoint := provider.EndpointKey(r.Link)
		prefix := provider.PrefixKey(r.Link)
		if endpoint != "" && endpointCount[endpoint] >= maxReadyPerEndpoint {
			continue
		}
		if prefix != "" && prefixCount[prefix] >= maxReadyPerPrefix {
			continue
		}
		add(r)
	}

	// Below-minimum fallback: an empty or short pool is worse than a duplicate
	// endpoint. This is the only path that relaxes the caps.
	if len(selected) < minReady {
		for _, r := range candidates {
			if len(selected) >= target {
				break
			}
			if _, ok := chosen[r.Link.Hash]; ok {
				continue
			}
			add(r)
		}
	}

	sortReady(selected)
	return selected
}

func diversityStats(ready []contract.Ready, shortfall bool, events uint64, last time.Time) contract.Diversity {
	counts := provider.Summarize(ready)
	return contract.Diversity{
		ReadyCloudflare:    counts.Cloudflare,
		ReadyNonCloudflare: counts.NonCloudflare,
		ReadyUnknown:       counts.Unknown,
		ReadyDomains:       counts.Domains,
		SingleProvider:     counts.SingleProvider,
		Shortfall:          shortfall,
		ShortfallEvents:    events,
		LastShortfall:      last,
	}
}

// readyLacksNonCloudflare reports whether the ready pool has no known
// non-Cloudflare standby. It is the trigger for the soft provider reserve.
func (m *Manager) readyLacksNonCloudflare() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !hasNonCloudflare(m.ready)
}

func (m *Manager) readyHasNonCloudflare() bool {
	return !m.readyLacksNonCloudflare()
}

// recordDiversityShortfall increments the shortfall event counter and logs one
// bounded event per reserve attempt that produced no non-Cloudflare standby.
func (m *Manager) recordDiversityShortfall(nonCFCandidates int) {
	now := m.now().UTC()
	m.mu.Lock()
	m.diversityShortfall = true
	m.diversityShortfallEvents++
	m.lastDiversityShortfall = now
	ready := len(m.ready)
	m.mu.Unlock()
	m.logger.Warn("wan: diversity_shortfall",
		"reason", "no non-Cloudflare standby qualified",
		"non_cloudflare_candidates", nonCFCandidates,
		"ready", ready)
}

func (m *Manager) clearDiversityShortfall() {
	m.mu.Lock()
	m.diversityShortfall = false
	m.mu.Unlock()
}
