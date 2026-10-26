package wan

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dissident/internal/contract"
	"dissident/internal/xraycfg"
)

// Replace performs a make-before-break promotion of the highest-score ready
// link. The old process is never stopped before the swap point; after the swap
// it is drained for swap.drain_timeout and then stopped.
//
// Only one Replace runs at a time. A failed candidate is removed from ready and
// marked failed in the catalog, then the next candidate is tried (the original
// list is walked at most once). With no promotable candidate a cycle is
// triggered asynchronously and an error is returned.
func (m *Manager) Replace(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.replaceMu.Lock()
	defer m.replaceMu.Unlock()

	if m.isStopping() {
		return errStopped
	}

	m.mu.Lock()
	candidates := cloneReady(m.ready)
	activeHash := ""
	if m.active != nil {
		activeHash = m.active.meta.Link.Hash
	}
	m.mu.Unlock()
	if len(candidates) == 0 {
		if !m.Cycling() {
			m.triggerCycle("replace: no ready candidate")
		}
		return errors.New("wan: replace: no ready candidate")
	}
	sortReady(candidates)

	cfg := m.currentConfig()
	promotionTimeout := cfg.Swap.PromotionTimeout
	if promotionTimeout <= 0 {
		promotionTimeout = 15 * time.Second
	}

	var lastErr error
	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cand.Link.Hash == "" || cand.Link.Hash == activeHash {
			continue
		}
		if !m.isReady(cand.Link.Hash) {
			// Another actor (cycle/RemoveReady) removed it while we queued.
			continue
		}
		if err := xraycfg.Supports(cand.Link); err != nil {
			lastErr = err
			m.dropReady(cand.Link, contract.Result{
				Hash: cand.Link.Hash, Stage: contract.StageParse, OK: false,
				Reason: err.Error(), At: m.now().UTC(),
			})
			continue
		}

		p, err := m.promotedOrVerified(ctx, cand.Link, promotionTimeout)
		if err != nil {
			lastErr = err
			m.logger.Warn("wan: promotion failed after verification fallback",
				"hash", cand.Link.Hash, "err", err)
			m.dropReady(cand.Link, contract.Result{
				Hash: cand.Link.Hash, Stage: contract.StageReal, OK: false,
				Reason: err.Error(), At: m.now().UTC(),
			})
			continue
		}
		if err := m.installActive(cand, p); err != nil {
			lastErr = err
			_ = m.stopProcess(context.Background(), p)
			m.dropReady(cand.Link, contract.Result{
				Hash: cand.Link.Hash, Stage: contract.StageReal, OK: false,
				Reason: err.Error(), At: m.now().UTC(),
			})
			// A shutdown raced the swap; do not try more candidates.
			if errors.Is(err, errStopped) {
				return err
			}
			continue
		}
		m.logger.Info("wan: replaced active", "hash", cand.Link.Hash, "port", p.Port())
		return nil
	}

	if !m.Cycling() {
		m.triggerCycle("replace: no candidate promoted")
	}
	if lastErr == nil {
		lastErr = errors.New("no ready candidate could be promoted")
	}
	return fmt.Errorf("wan: replace: %w", lastErr)
}

// installActive atomically swaps in a promoted process. The caller must hold
// replaceMu. The old process is scheduled for drain only after the active
// pointer has been swapped.
func (m *Manager) installActive(cand contract.Ready, p process) error {
	port := p.Port()
	if port <= 0 {
		return errors.New("wan: promoted process has no local port")
	}
	meta := contract.Active{
		Link:      cloneLink(cand.Link),
		Since:     m.now().UTC(),
		ExitIP:    cand.ExitIP,
		XrayPID:   p.PID(),
		LocalPort: port,
	}

	m.mu.Lock()
	if m.stopping || m.stopped {
		m.mu.Unlock()
		return errStopped
	}
	old := m.active
	m.active = &activeWAN{meta: meta, proc: p, routeID: routeIDFor(cand.Link.Hash, p.PID())}
	m.removeReadyLocked(cand.Link.Hash)
	m.probeFails = 0
	m.nextReplaceAt = time.Time{}
	m.failures = make(map[string][]time.Time)
	m.mu.Unlock()

	_ = m.saveState()
	m.markCatalog(cand.Link.Hash, contract.StateActive, contract.Result{
		Hash: cand.Link.Hash, Stage: contract.StageReal, OK: true,
		TTFB: 0, ExitIP: cand.ExitIP, At: m.now().UTC(),
	})

	if old != nil && old.proc != nil {
		m.scheduleDrain(old.proc)
	}
	return nil
}

// promoteReady is the cycle's serialized promotion path. It shares replaceMu
// with Replace so a cycle and a manual replacement can never install two
// active processes. It reports whether this call installed the active WAN;
// false with a nil error means another actor already did, so the candidate
// should remain a standby.
func (m *Manager) promoteReady(ctx context.Context, cand contract.Ready) (bool, error) {
	m.replaceMu.Lock()
	defer m.replaceMu.Unlock()
	if m.isStopping() {
		return false, errStopped
	}
	if m.hasActive() {
		return false, nil
	}
	promotionTimeout := m.currentConfig().Swap.PromotionTimeout
	if promotionTimeout <= 0 {
		promotionTimeout = 15 * time.Second
	}
	p, err := m.promotedOrVerified(ctx, cand.Link, promotionTimeout)
	if err != nil {
		return false, err
	}
	if err := m.installActive(cand, p); err != nil {
		_ = m.stopProcess(context.Background(), p)
		return false, err
	}
	return true, nil
}

// scheduleDrain keeps the old process alive for the configured drain timeout so
// in-flight connections that already hold its SOCKS address can finish. The
// drain is bounded by the manager lifetime and by shutdown.
func (m *Manager) scheduleDrain(p process) {
	drain := m.currentConfig().Swap.DrainTimeout
	if drain <= 0 {
		drain = 30 * time.Second
	}
	m.mu.Lock()
	if m.draining == nil {
		m.draining = make(map[int]process)
	}
	m.nextDrainID++
	id := m.nextDrainID
	m.draining[id] = p
	m.mu.Unlock()

	ok := m.launchTask("drain", func(ctx context.Context) {
		defer m.forgetDraining(id)
		timer := time.NewTimer(drain)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		stopCtx, cancel := boundedContext(context.Background(), defaultStopTimeout)
		if err := m.stopProcess(stopCtx, p); err != nil {
			m.logger.Warn("wan: draining old xray failed", "err", err)
		}
		cancel()
	})
	if !ok {
		// Shutdown refused the task; the caller's Stop path will not see this
		// process, so stop it now with a bounded context.
		m.forgetDraining(id)
		stopCtx, cancel := boundedContext(context.Background(), defaultStopTimeout)
		_ = m.stopProcess(stopCtx, p)
		cancel()
	}
}

func (m *Manager) forgetDraining(id int) {
	m.mu.Lock()
	delete(m.draining, id)
	m.mu.Unlock()
}

// dropReady removes a candidate from the ready list and marks it failed in the
// catalog. It persists the change.
func (m *Manager) dropReady(link contract.Link, res contract.Result) {
	m.mu.Lock()
	if m.active != nil && m.active.meta.Link.Hash == link.Hash {
		m.mu.Unlock()
		return
	}
	m.removeReadyLocked(link.Hash)
	m.mu.Unlock()
	m.markCatalog(link.Hash, contract.StateFailed, res)
	_ = m.saveState()
	m.setLastError(res.Reason)
}

// RemoveReady removes one ready standby by exact hash. Unknown hashes are an
// error so the control plane can report the stale action.
func (m *Manager) RemoveReady(hash string) error {
	if hash == "" {
		return errors.New("wan: remove ready: empty hash")
	}
	m.mu.Lock()
	if m.active != nil && m.active.meta.Link.Hash == hash {
		m.mu.Unlock()
		return errors.New("wan: remove ready: link is the active WAN")
	}
	found := false
	for _, r := range m.ready {
		if r.Link.Hash == hash {
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return fmt.Errorf("wan: remove ready: %q is not ready", hash)
	}
	m.removeReadyLocked(hash)
	m.mu.Unlock()
	if err := m.saveState(); err != nil {
		return err
	}
	m.logger.Info("wan: removed ready link", "hash", hash)
	m.maybeTriggerRefill("ready removed")
	return nil
}

func (m *Manager) isReady(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.ready {
		if r.Link.Hash == hash {
			return true
		}
	}
	return false
}

func (m *Manager) removeReadyLocked(hash string) {
	if hash == "" {
		return
	}
	out := m.ready[:0]
	for _, r := range m.ready {
		if r.Link.Hash != hash {
			out = append(out, r)
		}
	}
	m.ready = out
}

func (m *Manager) isStopping() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping || m.stopped
}

func (m *Manager) markCatalog(hash string, state contract.EntryState, res contract.Result) {
	if m.catalog == nil {
		return
	}
	m.catalog.Mark(hash, state, res)
}
