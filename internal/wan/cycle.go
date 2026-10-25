package wan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dissident/internal/catalog"
	"dissident/internal/contract"
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
	fetchTimeout := cfg.Test.HTTPTimeout
	if fetchTimeout <= 0 {
		fetchTimeout = 10 * time.Second
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

	links, sourceErrs, fetchErr := catalog.LoadSources(cycleCtx, cfg.Paths.URLsFile, fetchTimeout)
	if fetchErr != nil {
		// Missing/unreadable source file: keep serving and keep ready.
		cycleErr = fmt.Errorf("wan: cycle: load sources: %w", fetchErr)
		m.logger.Warn("wan: cycle source fetch failed; keeping last-known-good", "err", fetchErr)
	} else if len(sourceErrs) > 0 {
		for _, err := range sourceErrs {
			m.logger.Warn("wan: cycle source failed", "err", err)
		}
	}
	if len(links) > 0 {
		added := m.catalog.Merge(links)
		m.logger.Info("wan: cycle merged sources", "links", len(links), "added", added)
	}

	// Testing continues even after a fetch failure so pending catalog entries
	// can still fill the ready list or install an active WAN. Failed entries
	// remain Pending, so remember everything attempted in this cycle to make
	// progress finite and preserve the "pending exhausted" stop condition.
	attempted := make(map[string]struct{})
	for {
		if err := cycleCtx.Err(); err != nil {
			if cycleErr == nil {
				cycleErr = err
			}
			_ = m.saveState()
			return cycleErr
		}

		active := m.hasActive()
		m.mu.Lock()
		readyLen := len(m.ready)
		activeHash := ""
		if m.active != nil {
			activeHash = m.active.meta.Link.Hash
		}
		readyHashes := m.readyHashesLocked()
		m.mu.Unlock()
		if active && readyLen >= target {
			break
		}

		pending := m.catalog.Pending(0)
		var next *contract.Entry
		for i := range pending {
			e := pending[i]
			if e.Link.Hash == "" || e.Link.Hash == activeHash {
				continue
			}
			if _, ok := readyHashes[e.Link.Hash]; ok {
				continue
			}
			if _, ok := attempted[e.Link.Hash]; ok {
				continue
			}
			if active && readyLen >= target {
				break
			}
			next = &e
			break
		}
		if next == nil {
			break
		}
		entry := *next
		attempted[entry.Link.Hash] = struct{}{}

		m.markCatalog(entry.Link.Hash, contract.StateTesting, contract.Result{
			Hash: entry.Link.Hash, Stage: contract.StageReal, At: m.now().UTC(),
		})
		res := m.testOne(cycleCtx, entry.Link)
		if cycleCtx.Err() != nil {
			// The test was interrupted, so the entry must not stay in the
			// testing state (Pending would never select it again).
			if res.Hash == "" {
				res.Hash = entry.Link.Hash
			}
			res.OK = false
			if res.Reason == "" {
				res.Reason = "cycle canceled"
			}
			if res.At.IsZero() {
				res.At = m.now().UTC()
			}
			m.markCatalog(entry.Link.Hash, contract.StateFailed, res)
			if cycleErr == nil {
				cycleErr = cycleCtx.Err()
			}
			_ = m.saveState()
			return cycleErr
		}
		res.Hash = entry.Link.Hash
		if res.At.IsZero() {
			res.At = m.now().UTC()
		}
		if !res.OK {
			m.markCatalog(entry.Link.Hash, contract.StateFailed, res)
			if res.Reason != "" {
				m.setLastError(res.Reason)
			}
			continue
		}

		cand := m.readyFrom(entry, res)
		if !active && !m.hasActive() {
			// Promote the first passer immediately instead of waiting for the
			// rest of the cycle. If another actor won the race, keep this
			// candidate as a standby rather than dropping it.
			promoted, err := m.promoteReady(cycleCtx, cand)
			if err != nil {
				if cycleCtx.Err() != nil {
					m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
						Hash: entry.Link.Hash, Stage: contract.StageReal, OK: false,
						Reason: "cycle canceled", At: m.now().UTC(),
					})
					cycleErr = cycleCtx.Err()
					_ = m.saveState()
					return cycleErr
				}
				m.logger.Warn("wan: cycle promotion failed", "hash", entry.Link.Hash, "err", err)
				m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
					Hash: entry.Link.Hash, Stage: contract.StageReal, OK: false,
					Reason: err.Error(), At: m.now().UTC(),
				})
				continue
			}
			if promoted {
				continue
			}
		}

		m.addReady(cand, target)
		m.markCatalog(entry.Link.Hash, contract.StateReady, res)
	}

	_ = m.saveState()
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

func (m *Manager) addReady(cand contract.Ready, target int) {
	if cand.Link.Hash == "" {
		return
	}
	m.mu.Lock()
	if m.active != nil && m.active.meta.Link.Hash == cand.Link.Hash {
		m.mu.Unlock()
		return
	}
	replaced := false
	for i := range m.ready {
		if m.ready[i].Link.Hash == cand.Link.Hash {
			m.ready[i] = cand
			replaced = true
			break
		}
	}
	if !replaced {
		m.ready = append(m.ready, cand)
	}
	sortReady(m.ready)
	if target > 0 && len(m.ready) > target {
		m.ready = m.ready[:target]
	}
	m.mu.Unlock()
	_ = m.saveState()
}

func (m *Manager) readyHashesLocked() map[string]struct{} {
	out := make(map[string]struct{}, len(m.ready))
	for _, r := range m.ready {
		out[r.Link.Hash] = struct{}{}
	}
	return out
}
