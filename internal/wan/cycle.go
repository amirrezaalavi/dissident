package wan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/contract"
	"dissident/internal/provider"
)

// RunCycle fetches configured sources, merges links into the catalog, and tests
// pending entries in catalog order (TCP latency). It stops when there is an
// active WAN and ready_target standbys are available, when pending entries run
// out, or when the context is canceled.
//
// A source-fetch failure or a canceled cycle never tears down the active WAN
// and never clears the ready list: the last-known-good runtime is preserved.
func (m *Manager) RunCycle(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isStopping() {
		return errStopped
	}
	if !m.cycling.CompareAndSwap(false, true) {
		return errAlreadyCycling
	}

	cycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	m.cycleCancel = cancel
	if m.cancelRequest {
		m.cancelRequest = false
		cancel()
	}
	m.mu.Unlock()
	// Publish "no cycle running" under the same lock CancelCycle takes, so a
	// late cancellation can never leak into the next cycle.
	defer func() {
		m.mu.Lock()
		m.cycleCancel = nil
		m.cancelRequest = false
		m.cycling.Store(false)
		m.mu.Unlock()
	}()

	cfg := m.currentConfig()
	interval := cfg.Cycle.Interval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	target := cfg.Cycle.ReadyTarget
	if target < 1 {
		target = 1
	}
	fetchTimeout := cfg.Cycle.FetchTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = 20 * time.Second
	}

	var cycleErr error
	defer func() {
		now := m.now()
		m.mu.Lock()
		m.lastCycle = now
		m.nextCycle = now.Add(interval)
		if cycleErr != nil {
			m.lastError = cycleErr.Error()
		} else {
			m.lastError = ""
		}
		m.mu.Unlock()
	}()

	if m.catalog == nil {
		cycleErr = errors.New("wan: cycle: no catalog")
		return cycleErr
	}

	// A restart can restore ready standbys but fail to restore the persisted
	// active. Promote the best ready candidate before spending time fetching
	// and testing new sources, otherwise a ready WAN sits idle while the
	// proxy fails closed.
	if !m.hasActive() && m.readyCount() > 0 {
		if err := m.Replace(cycleCtx); err != nil && cycleCtx.Err() == nil {
			m.logger.Warn("wan: cycle could not promote an existing ready link", "err", err)
		}
	}

	links, sourceErrs, fetchErr := catalog.LoadSources(cycleCtx, cfg.Paths.URLsFile, fetchTimeout)
	if fetchErr != nil {
		// Missing/unreadable source file: keep serving and keep ready.
		cycleErr = fmt.Errorf("wan: cycle: load sources: %w", fetchErr)
		m.logger.Warn("wan: cycle source fetch failed; keeping last-known-good", "err", fetchErr)
	} else if len(sourceErrs) > 0 {
		// Large public subscriptions contain thousands of unsupported or
		// malformed lines. Log a bounded sample instead of one warning per
		// line on every cycle.
		const maxLoggedSourceErrors = 20
		limit := len(sourceErrs)
		if limit > maxLoggedSourceErrors {
			limit = maxLoggedSourceErrors
		}
		for _, err := range sourceErrs[:limit] {
			m.logger.Warn("wan: cycle source failed", "err", err)
		}
		if suppressed := len(sourceErrs) - limit; suppressed > 0 {
			m.logger.Warn("wan: cycle source errors suppressed", "suppressed", suppressed)
		}
	}
	if len(links) > 0 {
		added := m.catalog.Merge(links)
		m.logger.Info("wan: cycle merged sources", "links", len(links), "added", added)
	}

	if m.hasActive() && m.readyCount() >= target {
		// The ready target is met, but a Cloudflare-only pool may still be
		// missing the provider reserve. In that case run a bounded reserve
		// probe instead of returning early; otherwise the soft reserve could
		// never fire while readiness is healthy.
		if !m.readyLacksNonCloudflare() {
			m.logger.Info("wan: cycle already at ready target", "ready", m.readyCount(), "target", target)
			return nil
		}
	}

	reserveNeeded := m.readyLacksNonCloudflare()
	reserveOnly := reserveNeeded && m.hasActive() && m.readyCount() >= target

	// Build one finite queue for this cycle. Failed entries inside the retry
	// cooldown are left for a later cycle instead of being hammered by every
	// low-ready refill.
	m.mu.Lock()
	activeHash := ""
	if m.active != nil {
		activeHash = m.active.meta.Link.Hash
	}
	readyHashes := m.readyHashesLocked()
	m.mu.Unlock()

	now := m.now().UTC()
	cooldown := cfg.Cycle.FailedRetry
	if cooldown < 0 {
		cooldown = 0
	}
	pending := make([]contract.Entry, 0)
	for _, entry := range m.catalog.Pending(0) {
		if entry.Link.Hash == "" || entry.Link.Hash == activeHash {
			continue
		}
		if _, ok := readyHashes[entry.Link.Hash]; ok {
			continue
		}
		if entry.State == contract.StateFailed && cooldown > 0 && now.Sub(entry.Updated) < cooldown {
			continue
		}
		pending = append(pending, entry)
	}
	queue, reserveQueued := buildCycleQueue(pending, reserveNeeded, reserveOnly, diversityReserveBatch, cfg.Test.RealTestCap)

	workers := cfg.Test.CheapWorkers
	if workers < 2 {
		workers = 2
	}
	workers *= 2
	if realWorkers := cfg.Test.RealWorkers; realWorkers > workers {
		workers = realWorkers
	}
	if workers > 64 {
		workers = 64
	}
	m.logger.Info("wan: cycle testing",
		"queued", len(queue),
		"workers", workers,
		"cheap_workers", cfg.Test.CheapWorkers,
		"real_workers", cfg.Test.RealWorkers,
		"ready", m.readyCount(),
		"target", target,
		"provider_reserve", reserveNeeded,
		"reserve_only", reserveOnly,
		"non_cloudflare_queued", reserveQueued)

	type outcome struct {
		entry contract.Entry
		res   contract.Result
	}
	tasks := make(chan contract.Entry)
	results := make(chan outcome, workers)
	var workersWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		workersWG.Add(1)
		go func() {
			defer workersWG.Done()
			for entry := range tasks {
				results <- outcome{entry: entry, res: m.testOne(cycleCtx, entry.Link)}
			}
		}()
	}
	go func() {
		workersWG.Wait()
		close(results)
	}()

	stop := false
	canceled := false
	launched := 0
	inFlight := 0
	nonCFTested := 0
	for {
		for !stop && !canceled && launched < len(queue) && inFlight < workers {
			if err := cycleCtx.Err(); err != nil {
				canceled = true
				break
			}
			entry := queue[launched]
			launched++
			m.markCatalog(entry.Link.Hash, contract.StateTesting, contract.Result{
				Hash: entry.Link.Hash, Stage: contract.StageReal, At: m.now().UTC(),
			})
			select {
			case tasks <- entry:
				inFlight++
				if reserveNeeded && provider.ClassifyLink(entry.Link) == provider.NonCloudflare {
					nonCFTested++
				}
			case <-cycleCtx.Done():
				m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
					Hash: entry.Link.Hash, Stage: contract.StageReal, OK: false,
					Reason: "cycle canceled", At: m.now().UTC(),
				})
				canceled = true
			}
			if m.hasActive() && m.readyCount() >= target {
				stop = true
			}
		}

		if inFlight == 0 {
			break
		}
		out, ok := <-results
		if !ok {
			break
		}
		inFlight--
		if canceled || cycleCtx.Err() != nil {
			m.markCatalog(out.entry.Link.Hash, contract.StateFailed, contract.Result{
				Hash: out.entry.Link.Hash, Stage: contract.StageReal, OK: false,
				Reason: "cycle canceled", At: m.now().UTC(),
			})
			continue
		}
		m.applyCycleResult(cycleCtx, out.entry, out.res, target)
		if m.hasActive() && m.readyCount() >= target {
			stop = true
		}
	}

	close(tasks)
	workersWG.Wait()
	if cycleCtx.Err() != nil {
		canceled = true
		if cycleErr == nil {
			cycleErr = cycleCtx.Err()
		}
	}
	if canceled && cycleErr == nil {
		cycleErr = context.Canceled
	}

	_ = m.saveState()
	if reserveNeeded && !canceled {
		switch {
		case m.readyHasNonCloudflare():
			m.clearDiversityShortfall()
		case reserveQueued == 0 || nonCFTested == 0:
			// An empty queue is usually a cooldown/no-candidate no-op, not a
			// failed reserve attempt. Do not pollute the shortfall counter or
			// gauge when no non-Cloudflare candidate was actually tested.
			m.logger.Debug("wan: empty diversity reserve; no shortfall recorded",
				"queued", reserveQueued, "tested", nonCFTested)
		default:
			m.recordDiversityShortfall(reserveQueued, nonCFTested)
		}
	} else if m.readyHasNonCloudflare() {
		// A normal cycle may also have supplied a non-Cloudflare entry (for
		// example after a manual action); clear a stale shortfall flag.
		m.clearDiversityShortfall()
	}
	if !m.hasActive() {
		if cycleErr == nil {
			cycleErr = errors.New("wan: cycle: no active WAN available")
		}
	} else if cycleErr == nil && m.readyCount() < cfg.Cycle.ReadyMin {
		// Not an error: the cycle simply exhausted currently pending entries.
		m.logger.Info("wan: cycle finished below ready_min", "ready", m.readyCount(), "ready_min", cfg.Cycle.ReadyMin)
	}
	return cycleErr
}

// applyCycleResult installs one completed qualification result. It is called
// only by the cycle coordinator, so active/ready mutations stay serialized even
// though the tests themselves run concurrently.
func (m *Manager) applyCycleResult(ctx context.Context, entry contract.Entry, res contract.Result, target int) {
	if res.Hash == "" {
		res.Hash = entry.Link.Hash
	}
	if res.At.IsZero() {
		res.At = m.now().UTC()
	}
	if !res.OK {
		m.markCatalog(entry.Link.Hash, contract.StateFailed, res)
		if res.Reason != "" {
			m.setLastError(res.Reason)
		}
		return
	}

	cand := m.readyFrom(entry, res)
	if !m.hasActive() {
		promoted, err := m.promoteReady(ctx, cand)
		if err != nil {
			if ctx.Err() != nil {
				m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
					Hash: entry.Link.Hash, Stage: contract.StageReal, OK: false,
					Reason: "cycle canceled", At: m.now().UTC(),
				})
				return
			}
			m.logger.Warn("wan: cycle promotion failed", "hash", entry.Link.Hash, "err", err)
			m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
				Hash: entry.Link.Hash, Stage: contract.StageReal, OK: false,
				Reason: err.Error(), At: m.now().UTC(),
			})
			return
		}
		if promoted {
			return
		}
	}

	// Endpoint-aware admission decides whether this candidate earns a slot and
	// which existing standby it displaces. A qualified candidate that loses to
	// diversity caps stays in the catalog (not ready) for a later cycle rather
	// than being marked ready while absent from the pool.
	if !m.addReady(cand, target) {
		m.logger.Debug("wan: ready candidate not admitted by endpoint/provider caps",
			"hash", entry.Link.Hash, "ready", m.readyCount(), "target", target)
		m.markCatalog(entry.Link.Hash, contract.StateCatalog, res)
		return
	}
	m.markCatalog(entry.Link.Hash, contract.StateReady, res)
}

func (m *Manager) readyFrom(entry contract.Entry, res contract.Result) contract.Ready {
	score := res.SpeedMbps
	if score <= 0 {
		if res.TTFB > 0 {
			score = float64(time.Second) / float64(res.TTFB)
		} else if entry.Score > 0 {
			score = entry.Score
		} else if res.TCPLatency > 0 {
			score = float64(time.Second) / float64(res.TCPLatency)
		} else {
			score = 1
		}
	}
	check := res.At
	if check.IsZero() {
		check = m.now().UTC()
	}
	return contract.Ready{
		Link:      cloneLink(entry.Link),
		Score:     score,
		LastCheck: check,
		ExitIP:    res.ExitIP,
	}
}

// addReady applies endpoint-aware admission and persists the result. It
// reports whether the candidate earned a slot. A rejected candidate leaves the
// existing pool untouched.
func (m *Manager) addReady(cand contract.Ready, target int) bool {
	if cand.Link.Hash == "" {
		return false
	}
	cfg := m.currentConfig()
	minReady := cfg.Cycle.ReadyMin
	m.mu.Lock()
	if m.active != nil && m.active.meta.Link.Hash == cand.Link.Hash {
		m.mu.Unlock()
		return false
	}
	existing := make([]contract.Ready, 0, len(m.ready)+1)
	for _, r := range m.ready {
		if r.Link.Hash != cand.Link.Hash {
			existing = append(existing, r)
		}
	}
	activeEndpoint, activePrefix, activeHash := "", "", ""
	if m.active != nil {
		activeHash = m.active.meta.Link.Hash
		activeEndpoint = provider.EndpointKey(m.active.meta.Link)
		activePrefix = provider.PrefixKey(m.active.meta.Link)
	}
	selected := selectReadyWithCaps(existing, cand, target, minReady,
		activeEndpoint, activePrefix)

	selectedHashes := make(map[string]struct{}, len(selected))
	admitted := false
	for _, r := range selected {
		selectedHashes[r.Link.Hash] = struct{}{}
		if r.Link.Hash == cand.Link.Hash {
			admitted = true
		}
	}
	if !admitted {
		m.mu.Unlock()
		return false
	}
	evicted := make([]string, 0)
	for _, r := range existing {
		if _, ok := selectedHashes[r.Link.Hash]; !ok {
			evicted = append(evicted, r.Link.Hash)
		}
	}
	m.ready = selected
	m.mu.Unlock()

	// Policy eviction is not a failure. Mark the entry back to catalog outside
	// the manager lock so it remains eligible for Pending and can be retested
	// in a later cycle. Reclassify preserves the last result and score.
	for _, hash := range evicted {
		if hash == activeHash {
			continue
		}
		m.markCatalogState(hash, contract.StateCatalog, readyEvictionReason)
	}
	_ = m.saveState()
	return true
}

func (m *Manager) readyHashesLocked() map[string]struct{} {
	out := make(map[string]struct{}, len(m.ready))
	for _, r := range m.ready {
		out[r.Link.Hash] = struct{}{}
	}
	return out
}
