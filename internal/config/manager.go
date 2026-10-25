package config

import (
	"errors"
	"sync"
	"sync/atomic"
)

// Snapshot is one immutable generation of the configuration.
type Snapshot struct {
	Generation uint64
	Config     Config
}

// Clone returns a deep copy of the snapshot.
func (s *Snapshot) Clone() *Snapshot {
	if s == nil {
		return nil
	}
	return &Snapshot{Generation: s.Generation, Config: s.Config.Clone()}
}

// Manager holds the current configuration snapshot behind an atomic pointer.
// Readers are lock-free; Apply is serialized so generations are monotonic.
type Manager struct {
	mu  sync.Mutex
	cur atomic.Pointer[Snapshot]
}

// NewManager validates cfg and publishes it as generation 1.
func NewManager(cfg Config) (*Manager, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{}
	m.cur.Store(&Snapshot{Generation: 1, Config: cfg.Clone()})
	return m, nil
}

// Current returns the active snapshot. It never returns nil and callers must
// treat the returned value as read-only.
func (m *Manager) Current() *Snapshot {
	if m == nil {
		return nil
	}
	return m.cur.Load()
}

// Apply validates cfg and atomically publishes it as the next generation. On
// any validation error the previous snapshot and generation are left intact.
func (m *Manager) Apply(cfg Config) error {
	if m == nil {
		return errors.New("config: nil manager")
	}
	next := cfg.Clone()
	if err := next.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.cur.Load()
	var generation uint64 = 1
	if cur != nil {
		generation = cur.Generation + 1
	}
	m.cur.Store(&Snapshot{Generation: generation, Config: next})
	return nil
}
