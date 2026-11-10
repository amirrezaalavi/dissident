package wan

import (
	"sort"

	"dissident/internal/contract"
)

// excludedReason is stored on the catalog entry so the Tests page explains why
// a link that used to be ready is no longer in the pool.
const excludedReason = "removed from ready pool by operator"

// dedupeLinks returns cloned links with an empty hash dropped and duplicates
// collapsed, sorted by hash so persisted output is stable.
func dedupeLinks(links []contract.Link) []contract.Link {
	if len(links) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(links))
	out := make([]contract.Link, 0, len(links))
	for _, link := range links {
		if link.Hash == "" {
			continue
		}
		if _, ok := seen[link.Hash]; ok {
			continue
		}
		seen[link.Hash] = struct{}{}
		out = append(out, cloneLink(link))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// excludedLinksLocked returns a deep copy of the operator exclusions. The
// caller must hold m.mu.
func (m *Manager) excludedLinksLocked() []contract.Link {
	if len(m.excluded) == 0 {
		return nil
	}
	out := make([]contract.Link, 0, len(m.excluded))
	for _, link := range m.excluded {
		out = append(out, cloneLink(link))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// ExcludedLinks returns the operator-dropped links, sorted by hash.
func (m *Manager) ExcludedLinks() []contract.Link {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.excludedLinksLocked()
}

// excludedHashesLocked returns the exclusion set. The caller must hold m.mu.
func (m *Manager) excludedHashesLocked() map[string]struct{} {
	if len(m.excluded) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(m.excluded))
	for hash := range m.excluded {
		out[hash] = struct{}{}
	}
	return out
}

// isExcluded reports whether an operator dropped this link.
func (m *Manager) isExcluded(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.excluded[hash]
	return ok
}

// installPersistedExclusions restores the dropped-link set from disk and
// applies it to the ready list and catalog. The persisted active always wins
// over a stale exclusion: a link cannot be both serving and dropped.
func (m *Manager) installPersistedExclusions(ps persistedState) {
	excluded := make(map[string]contract.Link, len(ps.Excluded))
	for _, link := range dedupeLinks(ps.Excluded) {
		if ps.Active != nil && ps.Active.Link.Hash == link.Hash {
			continue
		}
		excluded[link.Hash] = link
	}

	m.mu.Lock()
	m.excluded = excluded
	if len(excluded) > 0 {
		out := m.ready[:0]
		for _, r := range m.ready {
			if _, skip := excluded[r.Link.Hash]; skip {
				continue
			}
			out = append(out, r)
		}
		m.ready = out
	}
	m.mu.Unlock()

	m.markExcludedInCatalog()
}

// markExcludedInCatalog reclassifies dropped links so the catalog view (and
// Pending) agrees with the exclusion set. It is safe to call when the catalog
// is empty: new entries merged later are reclassified at the start of every
// cycle.
func (m *Manager) markExcludedInCatalog() {
	if m.catalog == nil {
		return
	}
	for _, link := range m.ExcludedLinks() {
		m.catalog.Reclassify(link.Hash, contract.StateExcluded, excludedReason)
	}
}
