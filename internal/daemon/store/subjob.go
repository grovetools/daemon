package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
)

const joinedRetention = 30 * 24 * time.Hour

func subjobKey(planKey, childID string) string { return planKey + "/" + childID }

// ApplySubjobEventLocked folds an event. ApplyUpdate must hold the store lock.
func (s *Store) applySubjobEvent(ev *models.SubjobEvent) {
	if ev == nil {
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	key := subjobKey(ev.PlanKey, ev.ChildJobID)
	old := s.state.Subjobs[key]
	if old != nil {
		// A joined tombstone never regresses from a delayed ready frame. Digest
		// replacement requires a future explicit reconciliation proof rather
		// than trusting an otherwise indistinguishable producer retry.
		if old.State == models.SubjobJoined && ev.Kind == models.SubjobReportReady {
			return
		}
		if old.ReportSHA256 == ev.ReportSHA256 && old.State == ev.Kind {
			return
		}
		if old.ReportSHA256 != ev.ReportSHA256 && ev.Kind == models.SubjobJoined {
			return // acknowledgements may only claim the currently indexed report
		}
	}
	s.state.Subjobs[key] = &models.SubjobState{
		PlanKey: ev.PlanKey, ParentJobID: ev.ParentJobID, ChildJobID: ev.ChildJobID,
		State: ev.Kind, ReportSHA256: ev.ReportSHA256, UpdatedAt: ev.Timestamp,
	}
	s.persistSubjobs()
}

// GetSubjobSnapshot returns defensive copies filtered by exact owner identity.
func (s *Store) GetSubjobSnapshot(planKey, parentJobID string) *models.SubjobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := &models.SubjobSnapshot{Reports: make(map[string]*models.SubjobState)}
	for _, st := range s.state.Subjobs {
		if st.PlanKey != planKey || st.ParentJobID != parentJobID {
			continue
		}
		copy := *st
		out.Reports[st.ChildJobID] = &copy
	}
	return out
}

func subjobStatePath() string {
	return filepath.Join(paths.StateDir(), "daemon", "subjobs", "state.json")
}

// persistSubjobs publishes the latest map atomically. Failures are best effort.
func (s *Store) persistSubjobs() {
	cutoff := time.Now().Add(-joinedRetention)
	for k, st := range s.state.Subjobs {
		if st.State == models.SubjobJoined && st.UpdatedAt.Before(cutoff) {
			delete(s.state.Subjobs, k)
		}
	}
	data, err := json.Marshal(s.state.Subjobs)
	if err != nil {
		return
	}
	path := subjobStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		if err = os.Rename(name, path); err == nil {
			if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
				_ = dir.Sync()
				_ = dir.Close()
			}
		}
	}
}

func (s *Store) loadPersistedSubjobs() {
	data, err := os.ReadFile(subjobStatePath())
	if err != nil {
		return
	}
	var states map[string]*models.SubjobState
	if json.Unmarshal(data, &states) != nil {
		return
	}
	cutoff := time.Now().Add(-joinedRetention)
	for k, st := range states {
		if st == nil || (st.State == models.SubjobJoined && st.UpdatedAt.Before(cutoff)) {
			continue
		}
		s.state.Subjobs[k] = st
	}
}
