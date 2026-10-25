package wan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"dissident/internal/config"
	"dissident/internal/contract"
	"dissident/internal/platform"
)

const (
	defaultReadyLiveness = 100 * time.Second
	defaultTCPTimeout    = 3 * time.Second
)

// readyLivenessLoop periodically checks standby metadata with a cheap TCP
// dial. It never starts xray and never runs the full qualification pipeline.
func (m *Manager) readyLivenessLoop(ctx context.Context) {
	defer m.loopsWG.Done()
	interval := m.currentConfig().Cycle.ReadyLiveness
	if interval <= 0 {
		interval = defaultReadyLiveness
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkReadyOnce(ctx)
		}
	}
}

// checkReadyOnce performs one cheap TCP reachability pass over the current
// ready standbys. Failed entries are dropped and marked failed; successful
// entries get a fresh LastCheck. If the ready list is below ready_min, one
// asynchronous refill cycle is requested (deduplicated with the cycle guard).
func (m *Manager) checkReadyOnce(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.isStopping() {
		return
	}
	cfg := m.currentConfig()
	minReady := cfg.Cycle.ReadyMin
	if minReady < 1 {
		minReady = 1
	}

	dial, err := m.readyDialFunc(cfg)
	if err != nil {
		// A bad egress configuration is not a link failure: keep the ready
		// metadata and surface the error instead of dropping every standby.
		m.logger.Warn("wan: ready liveness cannot build dialer", "err", err)
		m.setLastError(err.Error())
		return
	}

	m.mu.Lock()
	standbys := cloneReady(m.ready)
	m.mu.Unlock()

	changed := false
	for _, entry := range standbys {
		if ctx.Err() != nil {
			break
		}
		addr, addrErr := readyAddress(entry.Link)
		var dialErr error
		if addrErr != nil {
			dialErr = addrErr
		} else {
			dctx, cancel := context.WithTimeout(ctx, tcpTimeout(cfg))
			conn, derr := dial(dctx, addr)
			cancel()
			if conn != nil {
				_ = conn.Close()
			}
			dialErr = derr
		}
		// A manager shutdown cancels the dial context; that is not a link
		// failure and must never wipe the ready list.
		if ctx.Err() != nil {
			break
		}

		now := m.now().UTC()
		if dialErr != nil {
			m.mu.Lock()
			removed := removeReadyIfPresentLocked(m.ready, entry.Link.Hash)
			if removed != nil {
				m.ready = removed
			}
			m.mu.Unlock()
			if removed == nil {
				continue
			}
			changed = true
			m.markCatalog(entry.Link.Hash, contract.StateFailed, contract.Result{
				Hash: entry.Link.Hash, Stage: contract.StageTCP, OK: false,
				Reason: dialErr.Error(), At: now,
			})
			m.logger.Warn("wan: ready link failed liveness; removed", "hash", entry.Link.Hash, "err", dialErr)
			continue
		}

		m.mu.Lock()
		for i := range m.ready {
			if m.ready[i].Link.Hash == entry.Link.Hash {
				m.ready[i].LastCheck = now
				changed = true
				break
			}
		}
		m.mu.Unlock()
	}

	if changed {
		if err := m.saveState(); err != nil {
			m.logger.Warn("wan: saving state after ready liveness failed", "err", err)
		}
	}
	if m.readyCount() < minReady {
		m.maybeTriggerRefill("ready below minimum")
	}
}

// readyDialFunc selects the configured transport. A configured egress proxy is
// mandatory: there is no direct fallback for ready checks either.
func (m *Manager) readyDialFunc(cfg config.Config) (func(context.Context, string) (net.Conn, error), error) {
	timeout := tcpTimeout(cfg)
	if proxy := strings.TrimSpace(cfg.Test.EgressProxy); proxy != "" {
		if m.egressDial == nil {
			return nil, errors.New("wan: no egress dialer configured")
		}
		dial, err := m.egressDial(proxy, timeout)
		if err != nil {
			return nil, err
		}
		if dial == nil {
			return nil, errors.New("wan: egress dialer is nil")
		}
		return func(ctx context.Context, address string) (net.Conn, error) {
			return dial(ctx, "tcp", address)
		}, nil
	}
	if m.dial == nil {
		return nil, errors.New("wan: no TCP dialer configured")
	}
	return func(ctx context.Context, address string) (net.Conn, error) {
		return m.dial(ctx, "tcp", address, timeout)
	}, nil
}

func tcpTimeout(cfg config.Config) time.Duration {
	if cfg.Test.TCPTimeout > 0 {
		return cfg.Test.TCPTimeout
	}
	return defaultTCPTimeout
}

func readyAddress(link contract.Link) (string, error) {
	host := strings.TrimSpace(link.Server)
	if host == "" {
		return "", errors.New("link has no server address")
	}
	if link.Port < 1 || link.Port > 65535 {
		return "", fmt.Errorf("link port %d out of range 1-65535", link.Port)
	}
	return net.JoinHostPort(host, strconv.Itoa(link.Port)), nil
}

// removeReadyIfPresentLocked removes the hash and reports whether anything was
// removed. The caller must hold m.mu.
func removeReadyIfPresentLocked(ready []contract.Ready, hash string) []contract.Ready {
	if hash == "" {
		return nil
	}
	found := false
	out := ready[:0]
	for _, r := range ready {
		if r.Link.Hash == hash {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return nil
	}
	return out
}

// maybeTriggerRefill starts one asynchronous cycle when ready is below
// ready_min. It deduplicates with an already-running cycle and with other
// refill requests, so a burst of ready failures starts at most one cycle.
func (m *Manager) maybeTriggerRefill(reason string) {
	minReady := m.currentConfig().Cycle.ReadyMin
	if minReady < 1 {
		minReady = 1
	}
	if m.readyCount() >= minReady {
		return
	}
	if m.Cycling() {
		return
	}
	if !m.refillScheduled.CompareAndSwap(false, true) {
		return
	}
	ok := m.launchTask("refill: "+reason, func(ctx context.Context) {
		defer m.refillScheduled.Store(false)
		if err := m.RunCycle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			m.logger.Warn("wan: ready refill cycle failed", "reason", reason, "err", err)
		}
	})
	if !ok {
		m.refillScheduled.Store(false)
	}
}

// verificationRestore starts a candidate xray and performs a verification-only
// 2xx check (no speed stage). It is the startup fallback when the full Promote
// path fails for a reason that is not necessarily the link itself.
func (m *Manager) verificationRestore(ctx context.Context, link contract.Link) (process, error) {
	if m.tester == nil {
		return nil, errors.New("wan: qualify tester is not configured")
	}
	cfg := m.currentConfig()
	port, err := platform.FreePort()
	if err != nil {
		return nil, fmt.Errorf("wan: restore fallback: allocate port: %w", err)
	}
	rendered, err := platform.RenderWithEgress(link, port, cfg.Test.EgressProxy)
	if err != nil {
		return nil, fmt.Errorf("wan: restore fallback: render: %w", err)
	}
	xray, err := platform.StartXray(ctx, cfg.Paths.XrayBin, rendered, "")
	if err != nil {
		return nil, fmt.Errorf("wan: restore fallback: start xray: %w", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(xray.Port))
	if err := m.tester.Verify(ctx, addr); err != nil {
		stopCtx, cancel := boundedContext(context.Background(), defaultStopTimeout)
		_ = xray.Stop(stopCtx)
		cancel()
		return nil, fmt.Errorf("wan: restore fallback: verify: %w", err)
	}
	return &platformProcess{x: xray}, nil
}
