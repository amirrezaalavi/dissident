// Package catalog stores deduplicated proxy links and their latest
// qualification results, and it loads share links from local files or remote
// subscriptions.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"sync"
	"time"

	"dissident/internal/contract"
)

// Store is a concurrency-safe catalog keyed by contract.Link.Hash.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*contract.Entry
}

// New returns an empty store.
func New() *Store {
	return &Store{entries: make(map[string]*contract.Entry)}
}

// Merge adds links whose hash is not already present and returns how many
// were added. Links with neither a hash nor raw text are ignored. Existing
// entries keep their state and last result.
func (s *Store) Merge(links []contract.Link) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*contract.Entry)
	}
	added := 0
	now := time.Now().UTC()
	for _, link := range links {
		link = withHash(link)
		if link.Hash == "" {
			continue
		}
		if _, exists := s.entries[link.Hash]; exists {
			continue
		}
		s.entries[link.Hash] = &contract.Entry{
			Link:    cloneLink(link),
			State:   contract.StateCatalog,
			Updated: now,
		}
		added++
	}
	return added
}

// Entries returns a deep copy of every entry, ordered by lifecycle state,
// then by score descending, then by most recently updated.
func (s *Store) Entries() []contract.Entry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]contract.Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		out = append(out, cloneEntry(*entry))
	}
	s.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		left, right := out[i], out[j]
		if leftRank, rightRank := stateRank(left.State), stateRank(right.State); leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.Score != right.Score {
			return left.Score > right.Score
		}
		if !left.Updated.Equal(right.Updated) {
			return left.Updated.After(right.Updated)
		}
		return left.Link.Hash < right.Link.Hash
	})
	return out
}

// Recent returns at most limit entries ordered for the control panel: state
// priority (testing, active, ready, catalog, failed, unsupported), then most
// recently updated, then score descending, then hash ascending. It scans the
// catalog and clones only the selected entries, so its cost is bounded by the
// map size plus limit rather than by a full-catalog deep copy. A limit of zero
// or less returns nil.
func (s *Store) Recent(limit int) []contract.Entry {
	return s.recent(limit, false)
}

// RecentTested returns at most limit entries that carry a test result, newest
// result first, then hash ascending. Like Recent, it clones only the selected
// entries. A limit of zero or less returns nil.
func (s *Store) RecentTested(limit int) []contract.Entry {
	return s.recent(limit, true)
}

// recent selects the top limit entries without materializing or sorting the
// whole catalog. The heap keeps the worst selected entry at index zero, so
// every non-selected map entry costs one comparison and no allocation.
func (s *Store) recent(limit int, testedOnly bool) []contract.Entry {
	if s == nil || limit <= 0 {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return nil
	}

	selected := make([]*contract.Entry, 0, limit)
	for _, entry := range s.entries {
		if entry == nil || (testedOnly && entry.Last.At.IsZero()) {
			continue
		}
		if len(selected) < limit {
			selected = append(selected, entry)
			if len(selected) == limit {
				buildWorstHeap(selected, testedOnly)
			}
			continue
		}
		if recentBefore(entry, selected[0], testedOnly) {
			selected[0] = entry
			siftDownWorst(selected, 0, testedOnly)
		}
	}
	if len(selected) == 0 {
		return nil
	}

	sort.Slice(selected, func(i, j int) bool {
		return recentBefore(selected[i], selected[j], testedOnly)
	})
	out := make([]contract.Entry, len(selected))
	for i, entry := range selected {
		out[i] = cloneEntry(*entry)
	}
	return out
}

// recentBefore reports whether a should be rendered before b.
func recentBefore(a, b *contract.Entry, testedOnly bool) bool {
	if testedOnly {
		if !a.Last.At.Equal(b.Last.At) {
			return a.Last.At.After(b.Last.At)
		}
		return a.Link.Hash < b.Link.Hash
	}
	if ra, rb := recentStateRank(a.State), recentStateRank(b.State); ra != rb {
		return ra < rb
	}
	ta, tb := recentEntryTime(a), recentEntryTime(b)
	if !ta.Equal(tb) {
		return ta.After(tb)
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.Link.Hash < b.Link.Hash
}

// recentEntryTime matches the control panel's notion of "most recently
// updated": the lifecycle timestamp when present, otherwise the last result.
func recentEntryTime(entry *contract.Entry) time.Time {
	if !entry.Updated.IsZero() {
		return entry.Updated
	}
	return entry.Last.At
}

// recentStateRank matches the /tests page order: testing first, then the
// serving states, then the remaining lifecycle states. Entries uses a
// different rank (active first) because it predates the panel view.
func recentStateRank(state contract.EntryState) int {
	switch state {
	case contract.StateTesting:
		return 0
	case contract.StateActive:
		return 1
	case contract.StateReady:
		return 2
	// Excluded entries are few and operator-relevant, so they stay above the
	// catalog instead of being pushed out of the bounded recent list.
	case contract.StateExcluded:
		return 3
	case contract.StateCatalog:
		return 4
	case contract.StateFailed:
		return 5
	case contract.StateUnsupported:
		return 6
	default:
		return 7
	}
}

// recentWorse reports whether a is worse than b, i.e. would render later.
func recentWorse(a, b *contract.Entry, testedOnly bool) bool {
	return recentBefore(b, a, testedOnly)
}

// buildWorstHeap arranges entries so the worst selected entry is at index 0.
func buildWorstHeap(entries []*contract.Entry, testedOnly bool) {
	for i := len(entries)/2 - 1; i >= 0; i-- {
		siftDownWorst(entries, i, testedOnly)
	}
}

func siftDownWorst(entries []*contract.Entry, i int, testedOnly bool) {
	n := len(entries)
	for {
		left := 2*i + 1
		if left >= n {
			return
		}
		worst := left
		if right := left + 1; right < n && recentWorse(entries[right], entries[left], testedOnly) {
			worst = right
		}
		if !recentWorse(entries[worst], entries[i], testedOnly) {
			return
		}
		entries[i], entries[worst] = entries[worst], entries[i]
		i = worst
	}
}

// Pending returns entries that still need qualification (catalog or failed),
// with coverage first: never-attempted entries, then least-recently-attempted
// retries. TCP latency is only a tie-break among entries attempted at the same
// time. A limit of zero or less returns every pending entry.
//
// Ordering by latency alone is unsafe for a large catalog: a small set of
// fast-but-dead targets keeps the head of the queue, so after every
// failed-retry cooldown the same candidates are retested and everything merged
// later is starved.
func (s *Store) Pending(limit int) []contract.Entry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]contract.Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.State == contract.StateCatalog || entry.State == contract.StateFailed {
			out = append(out, cloneEntry(*entry))
		}
	}
	s.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		left, right := out[i], out[j]
		// Untested entries first: coverage before retries.
		leftUntested := left.Attempts == 0
		rightUntested := right.Attempts == 0
		if leftUntested != rightUntested {
			return leftUntested
		}
		// Rotate retries by least-recently-attempted so a failed entry cannot
		// keep the head of the queue once its cooldown expires.
		if !left.Updated.Equal(right.Updated) {
			return left.Updated.Before(right.Updated)
		}
		// Latency remains a hint among entries attempted at the same time.
		leftZero := left.Last.TCPLatency <= 0
		rightZero := right.Last.TCPLatency <= 0
		if leftZero != rightZero {
			return !leftZero
		}
		if !leftZero && left.Last.TCPLatency != right.Last.TCPLatency {
			return left.Last.TCPLatency < right.Last.TCPLatency
		}
		return left.Link.Hash < right.Link.Hash
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// PendingNewest returns entries that still need qualification (catalog or
// failed), ordered by most recently Updated first, then by last TCP latency
// ascending (zero treated as the lowest hint so freshly merged entries stay
// near the front), then by hash. A limit of zero or less returns every pending
// entry. Like Pending, it clones entries under the read lock.
func (s *Store) PendingNewest(limit int) []contract.Entry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	out := make([]contract.Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.State == contract.StateCatalog || entry.State == contract.StateFailed {
			out = append(out, cloneEntry(*entry))
		}
	}
	s.mu.RUnlock()

	sort.SliceStable(out, func(i, j int) bool {
		left, right := out[i], out[j]
		if !left.Updated.Equal(right.Updated) {
			return left.Updated.After(right.Updated)
		}
		if left.Last.TCPLatency != right.Last.TCPLatency {
			return left.Last.TCPLatency < right.Last.TCPLatency
		}
		return left.Link.Hash < right.Link.Hash
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Mark records the outcome of one qualification attempt. The entry keeps its
// link; state, last result, score, attempt count, and updated time are
// replaced. Marking an unknown hash is a no-op.
func (s *Store) Mark(hash string, state contract.EntryState, res contract.Result) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[hash]
	if !ok {
		return
	}
	if res.Hash == "" {
		res.Hash = hash
	}
	entry.State = state
	entry.Last = res
	entry.Score = scoreOf(res)
	entry.Attempts++
	entry.Updated = time.Now().UTC()
}

// Reclassify changes only the lifecycle state of an entry, preserving its last
// result, score, and attempt count. It is used when an entry stops being ready
// for a policy reason (for example, ready-pool admission) rather than a
// qualification failure. A non-empty reason replaces the stored result reason so
// the panel explains the state change; an empty reason leaves the last result
// untouched. The updated time is refreshed so Pending can reconsider the entry.
// Reclassifying an unknown hash is a no-op and returns false.
func (s *Store) Reclassify(hash string, state contract.EntryState, reason string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[hash]
	if !ok {
		return false
	}
	entry.State = state
	if reason != "" {
		entry.Last.Reason = reason
	}
	entry.Updated = time.Now().UTC()
	return true
}

// Stats returns the number of entries, the number that are not unsupported,
// and the number currently marked testing.
func (s *Store) Stats() (total, supported, testing int) {
	if s == nil {
		return 0, 0, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	total = len(s.entries)
	for _, entry := range s.entries {
		if entry.State != contract.StateUnsupported {
			supported++
		}
		if entry.State == contract.StateTesting {
			testing++
		}
	}
	return total, supported, testing
}

// withHash fills a missing hash from Raw so manually built links still
// deduplicate. Parsed links already carry a hash.
func withHash(link contract.Link) contract.Link {
	if link.Hash != "" || link.Raw == "" {
		return link
	}
	sum := sha256.Sum256([]byte(link.Raw))
	link.Hash = hex.EncodeToString(sum[:])
	return link
}

// stateRank orders entries for display: serving candidates first, then the
// remaining lifecycle states.
func stateRank(state contract.EntryState) int {
	switch state {
	case contract.StateActive:
		return 0
	case contract.StateReady:
		return 1
	case contract.StateTesting:
		return 2
	case contract.StateExcluded:
		return 3
	case contract.StateCatalog:
		return 4
	case contract.StateFailed:
		return 5
	case contract.StateUnsupported:
		return 6
	default:
		return 7
	}
}

// scoreOf ranks a successful result. Speed dominates (Mbps capped at 1000),
// with up to 10 points for TTFB and 5 for TCP latency. Failures score zero.
func scoreOf(res contract.Result) float64 {
	if !res.OK {
		return 0
	}
	score := math.Min(math.Max(res.SpeedMbps, 0), 1000)
	if res.TTFB > 0 {
		ms := float64(res.TTFB) / float64(time.Millisecond)
		if ms < 2000 {
			score += 10 * (1 - ms/2000)
		}
	}
	if res.TCPLatency > 0 {
		ms := float64(res.TCPLatency) / float64(time.Millisecond)
		if ms < 1000 {
			score += 5 * (1 - ms/1000)
		}
	}
	if score == 0 {
		score = 1
	}
	return score
}

func cloneEntry(entry contract.Entry) contract.Entry {
	entry.Link = cloneLink(entry.Link)
	return entry
}

func cloneLink(link contract.Link) contract.Link {
	if link.Params != nil {
		params := make(map[string]string, len(link.Params))
		for key, value := range link.Params {
			params[key] = value
		}
		link.Params = params
	}
	return link
}
