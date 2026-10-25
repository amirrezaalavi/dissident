package dashboard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/yolka-wiz/viber-console/internal/collector"
)

// Store holds the latest snapshot of each daemon and refreshes it on a poll
// interval. The dashboard handlers read from the store, never from the
// daemons directly, so a slow or dead daemon degrades the UI instead of
// hanging it.
type Store struct {
	mu          sync.RWMutex
	interval    time.Duration
	vd          *collector.ViberaydClient
	vx          *collector.ViberoxyClient
	vdStats     *collector.ViberaydStats
	vdReachable bool
	vdSubURL    string
	vxSnapshot  *collector.ViberoxySnapshot
	vxHealth    string
	vxReady     string
	vxReachable bool
	lastPoll    time.Time
}

func NewStore(interval time.Duration, vd *collector.ViberaydClient, vx *collector.ViberoxyClient) *Store {
	return &Store{
		interval: interval,
		vd:       vd,
		vx:       vx,
	}
}

// Run polls both daemons until ctx is cancelled. Call in a goroutine.
func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.poll(ctx) // immediate first poll
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.poll(ctx)
		}
	}
}

func (s *Store) poll(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// Viberayd
	stats, err := s.vd.FetchStats(ctx)
	if err != nil {
		s.vdReachable = false
		slog.Warn("viberayd poll failed", "error", err)
	} else {
		s.vdStats = &stats
		s.vdReachable = true
		s.vdSubURL = s.vd.SubURL()
	}

	// Viberoxy
	snap, err := s.vx.FetchMetrics(ctx)
	if err != nil {
		s.vxReachable = false
		slog.Warn("viberoxy poll failed", "error", err)
	} else {
		s.vxSnapshot = &snap
		s.vxReachable = true
		if h, herr := s.vx.Healthz(ctx); herr == nil {
			s.vxHealth = h
		}
		if r, rerr := s.vx.Readyz(ctx); rerr == nil {
			s.vxReady = r
		}
	}

	s.lastPoll = now
}

// Snapshot is the immutable view handlers render from.
type Snapshot struct {
	GeneratedAt time.Time
	Viberayd    *collector.ViberaydStats
	ViberaydUp  bool
	SubURL      string
	Viberoxy    *collector.ViberoxySnapshot
	ViberoxyUp  bool
	ViberoxyHealth string
	ViberoxyReady  string
}

func (s *Store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Snapshot{
		GeneratedAt:    s.lastPoll,
		Viberayd:       s.vdStats,
		ViberaydUp:     s.vdReachable,
		SubURL:         s.vdSubURL,
		Viberoxy:       s.vxSnapshot,
		ViberoxyUp:     s.vxReachable,
		ViberoxyHealth: s.vxHealth,
		ViberoxyReady:  s.vxReady,
	}
}

// FetchConfigs proxies the viberayd config table through the store's client
// (live request; used by the configs endpoint with pagination/filter).
func (s *Store) FetchConfigs(ctx context.Context, page, perPage int, state string) (collector.ViberaydConfigPage, error) {
	return s.vd.FetchConfigs(ctx, page, perPage, state)
}

// FetchURLs proxies the viberayd URL list.
func (s *Store) FetchURLs(ctx context.Context) ([]string, error) {
	return s.vd.FetchURLs(ctx)
}

// ReplaceURLs proxies a full-list replace of viberayd subscription URLs.
func (s *Store) ReplaceURLs(ctx context.Context, want []string) ([]string, error) {
	return s.vd.ReplaceURLs(ctx, want)
}

// FetchWANSlots proxies the per-slot WAN state from viberoxy.
func (s *Store) FetchWANSlots(ctx context.Context) ([]byte, int, error) {
	return s.vx.FetchWANSlots(ctx)
}

// FetchCandidates proxies the candidate pool from viberoxy.
func (s *Store) FetchCandidates(ctx context.Context) ([]byte, int, error) {
	return s.vx.FetchCandidates(ctx)
}

// DropWAN proxies a drop request to viberoxy for the given WAN slot index.
func (s *Store) DropWAN(ctx context.Context, index int) ([]byte, int, error) {
	return s.vx.DropWAN(ctx, index)
}

// TriggerCycle proxies a manual cycle trigger to viberoxy.
func (s *Store) TriggerCycle(ctx context.Context) ([]byte, int, error) {
	return s.vx.TriggerCycle(ctx)
}
