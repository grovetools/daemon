package telemetry

import (
	"sort"
	"sync"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// warningTTL is how long a raised warning stays active without being
// re-raised. Health warnings are LEVEL-triggered, not edge-triggered: a rule
// re-raises its warning every time it observes the bad condition, and the
// condition self-clears simply by not recurring. That inverts the usual
// raise/clear bookkeeping — no recorder has to prove a problem went away from
// inside a hot path (which is exactly the bookkeeping that makes alert stores
// go stale and lie).
//
// Five minutes is long enough to survive a quiet interval between two git
// sweeps of a slow repo, short enough that a fixed .gitignore makes the
// warning disappear while the user is still looking at the screen.
const warningTTL = 5 * time.Minute

// warningCap bounds the ledger so a pathological rule (one warning per
// workspace on a machine with hundreds) cannot grow the daemon's heap. The
// oldest-seen entries are evicted first.
const warningCap = 64

// WarningLedger holds the daemon's active health warnings — doc 50 Layer 2's
// "minimal store surface: path, condition, offender, since". It is the state
// behind SystemStats.Warnings.
//
// Identity is (path, condition): the same rule firing again for the same
// workspace refreshes the existing entry and PRESERVES its Since, so the UI
// can say "slow since 14:02" rather than resetting the clock on every scan.
// Only the offender is updated, because the biggest file in a repo can change
// while the condition holds.
type WarningLedger struct {
	mu      sync.Mutex
	entries map[warningKey]*warningEntry
}

type warningKey struct{ path, condition string }

type warningEntry struct {
	offender string
	since    time.Time
	lastSeen time.Time
}

// NewWarningLedger returns an empty ledger.
func NewWarningLedger() *WarningLedger {
	return &WarningLedger{entries: map[warningKey]*warningEntry{}}
}

// Raise records that condition currently holds for path, attributed to
// offender. Calling it repeatedly is the intended usage (see warningTTL).
func (l *WarningLedger) Raise(path, condition, offender string) {
	l.raiseAt(path, condition, offender, time.Now())
}

func (l *WarningLedger) raiseAt(path, condition, offender string, now time.Time) {
	if l == nil || condition == "" {
		return
	}
	key := warningKey{path: path, condition: condition}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok {
		e.offender = offender
		e.lastSeen = now
		return
	}
	l.evictLocked(now)
	l.entries[key] = &warningEntry{offender: offender, since: now, lastSeen: now}
}

// Clear removes a warning immediately, for rules that can positively observe
// recovery (most cannot — they rely on the TTL).
func (l *WarningLedger) Clear(path, condition string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.entries, warningKey{path: path, condition: condition})
	l.mu.Unlock()
}

// Active returns the warnings still within the TTL, newest condition first
// (ties broken by path) so the strip's ordering is stable across polls.
// Expired entries are dropped as a side effect. Never returns nil.
func (l *WarningLedger) Active() []models.HealthWarning {
	return l.activeAt(time.Now())
}

func (l *WarningLedger) activeAt(now time.Time) []models.HealthWarning {
	out := []models.HealthWarning{}
	if l == nil {
		return out
	}
	l.mu.Lock()
	for key, e := range l.entries {
		if now.Sub(e.lastSeen) > warningTTL {
			delete(l.entries, key)
			continue
		}
		out = append(out, models.HealthWarning{
			Path:      key.path,
			Condition: key.condition,
			Offender:  e.offender,
			Since:     e.since,
		})
	}
	l.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.After(out[j].Since)
		}
		if out[i].Condition != out[j].Condition {
			return out[i].Condition < out[j].Condition
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// evictLocked makes room when the ledger is full by dropping expired entries
// first and then the least recently seen one.
func (l *WarningLedger) evictLocked(now time.Time) {
	if len(l.entries) < warningCap {
		return
	}
	for key, e := range l.entries {
		if now.Sub(e.lastSeen) > warningTTL {
			delete(l.entries, key)
		}
	}
	for len(l.entries) >= warningCap {
		var oldestKey warningKey
		var oldest time.Time
		first := true
		for key, e := range l.entries {
			if first || e.lastSeen.Before(oldest) {
				oldestKey, oldest, first = key, e.lastSeen, false
			}
		}
		delete(l.entries, oldestKey)
	}
}
