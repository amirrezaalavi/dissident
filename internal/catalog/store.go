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

// Pending returns entries that still need qualification (catalog or failed),
// ordered by last TCP latency ascending with zero latency last, then by oldest
// Updated. A limit of zero or less returns every pending entry.
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
		leftZero := left.Last.TCPLatency <= 0
		rightZero := right.Last.TCPLatency <= 0
		if leftZero != rightZero {
			return !leftZero
		}
		if !leftZero && left.Last.TCPLatency != right.Last.TCPLatency {
			return left.Last.TCPLatency < right.Last.TCPLatency
		}
		if !left.Updated.Equal(right.Updated) {
			return left.Updated.Before(right.Updated)
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
	case contract.StateCatalog:
		return 3
	case contract.StateFailed:
		return 4
	case contract.StateUnsupported:
		return 5
	default:
		return 6
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
