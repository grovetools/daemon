package server

import (
	"sync"
	"time"
)

// Retention bounds for agent "death rattle" snapshots. Both live capture
// tiers need a running PTY and a mounted pane, so an agent that dies during
// startup takes its error text with it: tier 1 answers "pty session not
// found" and tier 2's panel has already emitted AgentPanelExitedMsg. The pane
// therefore pushes its last screen here the instant the session exits, and
// capture falls back to it.
//
// The bounds are deliberately mean. This exists to explain a death that just
// happened, not to archive agent output, and groved outlives every plan that
// runs under it — an unbounded map keyed by job ID is a leak.
const (
	// finalOutputMaxBytes caps one snapshot. Trimming keeps the TAIL: a crash
	// prints its cause last, so the end of the buffer is the part worth having.
	finalOutputMaxBytes = 256 << 10
	// finalOutputMaxEntries caps how many jobs are retained at once, so a plan
	// churning through short-lived agents cannot grow the map without limit.
	finalOutputMaxEntries = 64
	// finalOutputTTL bounds retention in time. Flow captures seconds after the
	// exit; anything asking much later is not diagnosing this death.
	finalOutputTTL = 30 * time.Minute
)

type finalOutputEntry struct {
	text     string
	storedAt time.Time
}

// finalOutputStore retains agent panes' final screen text keyed by job ID,
// bounded in size, age, and count. Safe for concurrent use.
type finalOutputStore struct {
	mu      sync.Mutex
	entries map[string]finalOutputEntry
	// order is insertion order, used for FIFO eviction once maxEntries is hit.
	order      []string
	ttl        time.Duration
	maxEntries int
	maxBytes   int
	// now is overridable so TTL expiry is testable without sleeping.
	now func() time.Time
}

func newFinalOutputStore() *finalOutputStore {
	return &finalOutputStore{
		entries:    make(map[string]finalOutputEntry),
		ttl:        finalOutputTTL,
		maxEntries: finalOutputMaxEntries,
		maxBytes:   finalOutputMaxBytes,
		now:        time.Now,
	}
}

// Put retains text for jobID, replacing any previous snapshot for that job.
// Empty text is ignored so a blank screen never masks a real earlier capture.
func (s *finalOutputStore) Put(jobID, text string) {
	if jobID == "" || text == "" {
		return
	}
	if len(text) > s.maxBytes {
		text = text[len(text)-s.maxBytes:]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.purgeExpiredLocked(now)

	if _, existed := s.entries[jobID]; !existed {
		s.order = append(s.order, jobID)
	}
	s.entries[jobID] = finalOutputEntry{text: text, storedAt: now}

	for len(s.entries) > s.maxEntries && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, oldest)
	}
}

// Get returns the retained snapshot for jobID, dropping it if it has aged
// past the TTL.
func (s *finalOutputStore) Get(jobID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(s.now())
	e, ok := s.entries[jobID]
	if !ok {
		return "", false
	}
	return e.text, true
}

// Len reports how many snapshots are currently retained (post-expiry).
func (s *finalOutputStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(s.now())
	return len(s.entries)
}

func (s *finalOutputStore) purgeExpiredLocked(now time.Time) {
	if len(s.entries) == 0 {
		return
	}
	kept := s.order[:0]
	for _, id := range s.order {
		e, ok := s.entries[id]
		if !ok {
			continue
		}
		if now.Sub(e.storedAt) > s.ttl {
			delete(s.entries, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}
