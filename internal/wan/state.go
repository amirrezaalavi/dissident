package wan

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dissident/internal/contract"
)

// persistedState is the versioned on-disk representation of the WAN runtime.
// It deliberately stores raw links and promotion metadata only; process state
// is reconstructed by Tester.Promote on the next start.
type persistedState struct {
	Version int              `json:"version"`
	Active  *contract.Active `json:"active,omitempty"`
	Ready   []contract.Ready `json:"ready,omitempty"`
	SavedAt time.Time        `json:"saved_at,omitempty"`
}

const (
	stateVersion      = 1
	stateFileMode     = 0o600
	stateDirMode      = 0o755
	stateBackupSuffix = ".bak"
)

// statePath returns the configured state file. Options.StateFile wins so the
// manager can be tested with a temporary file independent of the config.
func (m *Manager) statePath() string {
	if m.stateFile != "" {
		return m.stateFile
	}
	return m.currentConfig().Paths.StateFile
}

// loadState reads and validates the persisted state. A missing, corrupt, or
// unsupported state file is non-fatal: callers get an empty state plus a log
// message, and the manager starts from scratch.
func (m *Manager) loadState() persistedState {
	var out persistedState
	path := m.statePath()
	if path == "" {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.logger.Warn("wan: cannot read state file; starting empty", "path", path, "err", err)
		}
		return out
	}
	defer f.Close()

	dec := json.NewDecoder(io.LimitReader(f, 16<<20))
	if err := dec.Decode(&out); err != nil {
		m.logger.Warn("wan: corrupt state file; starting empty", "path", path, "err", err)
		return persistedState{}
	}
	if out.Version != stateVersion {
		m.logger.Warn("wan: unsupported state version; starting empty", "path", path, "version", out.Version)
		return persistedState{}
	}
	if out.Active != nil && out.Active.Link.Hash == "" {
		m.logger.Warn("wan: persisted active entry has no hash; ignoring it", "path", path)
		out.Active = nil
	}
	out.Ready = dedupeReady(out.Ready)
	sortReady(out.Ready)
	return out
}

// saveState atomically writes the current active and ready metadata. The file
// is 0600 and replaced via a same-directory temporary file + rename, with a
// best-effort backup of the previous generation.
func (m *Manager) saveState() error {
	// Serialize writers as well as individual renames: without this, a stale
	// snapshot could be installed after a newer one by a concurrent actor.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	path := m.statePath()
	if path == "" {
		return nil
	}

	m.mu.Lock()
	ps := persistedState{
		Version: stateVersion,
		Active:  m.activeMetaLocked(),
		Ready:   cloneReady(m.ready),
		SavedAt: m.now().UTC(),
	}
	m.mu.Unlock()

	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("wan: marshal state: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return fmt.Errorf("wan: create state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".dissident-state-*.tmp")
	if err != nil {
		return fmt.Errorf("wan: create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(stateFileMode); err != nil {
		cleanup()
		return fmt.Errorf("wan: chmod temporary state file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("wan: write temporary state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("wan: sync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("wan: close temporary state file: %w", err)
	}

	backupState(path)
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("wan: install state file: %w", err)
	}
	// Rename preserves the temporary file's mode, but chmod defensively in
	// case the filesystem or a previous state file imposed another mode.
	if err := os.Chmod(path, stateFileMode); err != nil {
		return fmt.Errorf("wan: chmod state file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// backupState copies the previous state to <path>.bak at 0600. Failure is not
// fatal: the new state is still installed atomically.
func backupState(path string) {
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dissident-state-bak-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if err := tmp.Chmod(stateFileMode); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if _, err := io.Copy(tmp, io.LimitReader(src, 16<<20)); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, path+stateBackupSuffix); err != nil {
		os.Remove(name)
	}
}

// activeMetaLocked returns a copy of the active metadata for persistence.
// The caller must hold m.mu.
func (m *Manager) activeMetaLocked() *contract.Active {
	if m.active == nil {
		return nil
	}
	meta := m.active.meta
	meta.Link = cloneLink(meta.Link)
	return &meta
}

func dedupeReady(in []contract.Ready) []contract.Ready {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]contract.Ready, 0, len(in))
	for _, r := range in {
		if r.Link.Hash == "" {
			continue
		}
		if _, ok := seen[r.Link.Hash]; ok {
			continue
		}
		seen[r.Link.Hash] = struct{}{}
		r.Link = cloneLink(r.Link)
		out = append(out, r)
	}
	return out
}

func sortReady(ready []contract.Ready) {
	sort.SliceStable(ready, func(i, j int) bool {
		if ready[i].Score == ready[j].Score {
			return ready[i].Link.Hash < ready[j].Link.Hash
		}
		return ready[i].Score > ready[j].Score
	})
}
