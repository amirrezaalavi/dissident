// Package app wires the process together: module lifecycle, ordered start and
// stop, transactional config reload, and the control-plane listener.
package app

import (
	"context"
	"time"

	"dissident/internal/config"
)

// Module is one in-process dissident subsystem. Start must return once the
// module is ready (or failed), not block for its lifetime. Reload is
// transactional: validate first and leave runtime state untouched on error.
// Stop is idempotent and bounded by the supplied context.
type Module interface {
	Name() string
	Start(ctx context.Context) error
	Reload(s *config.Snapshot) error
	Stop(ctx context.Context) error
}

// ReloadValidator is an optional capability. The supervisor calls every
// module that implements it before mutating any module, so an invalid reload
// is rejected before the first change.
type ReloadValidator interface {
	ValidateReload(s *config.Snapshot) error
}

// Health status values used by HealthReporter.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
	HealthFailed   = "failed"
)

// Health is an optional module health view for the control plane.
type Health struct {
	Status string
	Detail string
	At     time.Time
}

// HealthReporter is an optional capability for modules with health state.
type HealthReporter interface {
	Health() Health
}

// SnapshotProvider is an optional capability for modules whose state should
// be visible to the control plane.
type SnapshotProvider interface {
	Snapshot() any
}
