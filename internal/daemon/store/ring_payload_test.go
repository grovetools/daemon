package store

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// TestRingDropsWholesaleReplacementPayloads is the memory half of the contract:
// a superseded note index must not stay reachable through the replay ring. The
// collector rebuilds all ~38k entries every 5 minutes, so a ring that retained
// each published generation pinned ~12 of them (244 MB on the live daemon).
func TestRingDropsWholesaleReplacementPayloads(t *testing.T) {
	s := New()

	// Two DIFFERENT indexes, so the digest fence lets both through.
	first := map[string]*models.NoteIndexEntry{"/nb/notes/a.md": {Path: "/nb/notes/a.md"}}
	second := map[string]*models.NoteIndexEntry{"/nb/notes/b.md": {Path: "/nb/notes/b.md"}}
	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: first})
	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: second})

	replay, gap := s.Replay(0)
	if gap.Reason != "" {
		t.Fatalf("unexpected replay gap %q", gap.Reason)
	}
	if len(replay) != 2 {
		t.Fatalf("replayed %d updates, want 2 — the ring must still record the events", len(replay))
	}
	for i, u := range replay {
		if u.Type != UpdateNoteIndex {
			t.Errorf("replay[%d].Type = %q, want %q", i, u.Type, UpdateNoteIndex)
		}
		if u.Seq != uint64(i+1) {
			t.Errorf("replay[%d].Seq = %d, want %d — dropping a payload must not disturb sequencing", i, u.Seq, i+1)
		}
		if u.Source != "note" {
			t.Errorf("replay[%d].Source = %q, want \"note\"", i, u.Source)
		}
		if u.Payload != nil {
			t.Errorf("replay[%d] retained its payload; the ring would pin every superseded index generation", i)
		}
	}

	// The state still holds the CURRENT index: dropping the ring copy must not
	// cost the store the generation it actually serves.
	if got := len(s.Get().NoteIndex); got != len(second) {
		t.Errorf("state note index has %d entries, want %d", got, len(second))
	}
	if _, ok := s.Get().NoteIndex["/nb/notes/b.md"]; !ok {
		t.Error("state note index lost the newest generation")
	}
}

// TestRingKeepsOrdinaryPayloads is the fence's other side: the drop applies to
// the declared wholesale-replacement types ONLY. A ?since= client still replays
// the per-entity events it reconnects for.
func TestRingKeepsOrdinaryPayloads(t *testing.T) {
	s := New()
	job := &models.JobInfo{ID: "job-1"}
	s.ApplyUpdate(Update{Type: UpdateJobStarted, Source: "test", Payload: job})

	replay, _ := s.Replay(0)
	if len(replay) != 1 {
		t.Fatalf("replayed %d updates, want 1", len(replay))
	}
	got, ok := replay[0].Payload.(*models.JobInfo)
	if !ok {
		t.Fatalf("replayed payload is %T, want *models.JobInfo", replay[0].Payload)
	}
	if got.ID != "job-1" {
		t.Errorf("replayed job ID = %q, want job-1", got.ID)
	}
}

// TestRingDropSetIsWholesaleReplacementOnly pins the set itself. Adding a type
// here is only safe when its payload is a wholesale replacement the store fully
// absorbs in ApplyUpdate AND the wire shape carries no payload; the server-side
// half of that check is TestRingDropsAreUnreachableOnTheWire.
func TestRingDropSetIsWholesaleReplacementOnly(t *testing.T) {
	want := map[UpdateType]bool{
		UpdateNoteIndex:         true,
		UpdatePlans:             true,
		UpdatePlanIndexSnapshot: true,
	}
	for typ := range want {
		if !RingDropsPayload(typ) {
			t.Errorf("RingDropsPayload(%q) = false, want true", typ)
		}
	}
	for _, typ := range []UpdateType{
		UpdateWorkspaces, UpdateWorkspacesDelta, UpdateSessions, UpdateNoteEvent,
		UpdateJobStarted, UpdatePlanIndexDelta, UpdateNavBindings,
	} {
		if RingDropsPayload(typ) {
			t.Errorf("RingDropsPayload(%q) = true; that type's payload IS read off the ring", typ)
		}
	}
}
