package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func noteIndexFixture(n int) map[string]*models.NoteIndexEntry {
	base := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	index := make(map[string]*models.NoteIndexEntry, n)
	for i := range n {
		key := fmt.Sprintf("/nb/notes/%03d.md", i)
		index[key] = &models.NoteIndexEntry{
			Path:      key,
			Name:      fmt.Sprintf("%03d.md", i),
			Title:     fmt.Sprintf("note %d", i),
			Tags:      []string{"alpha", "beta"},
			Type:      "note",
			Group:     "inbox",
			Workspace: "grovetools",
			ModTime:   base.Add(time.Duration(i) * time.Minute),
		}
	}
	return index
}

// TestNoteIndexRepublishIsSuppressed is the fence's whole point: the note
// producers rebuild the entire index every scan, and an identical rebuild must
// not reach the replay ring, which retains payloads by reference.
func TestNoteIndexRepublishIsSuppressed(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer s.Unsubscribe(sub)

	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: noteIndexFixture(50)})
	first := s.CurrentSeq()
	if first == 0 {
		t.Fatal("the first note index did not publish")
	}
	drain(sub)

	// A byte-for-byte equivalent rebuild — a different map with the same
	// contents, which is exactly what the next scan produces.
	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: noteIndexFixture(50)})
	if got := s.CurrentSeq(); got != first {
		t.Errorf("an unchanged index advanced the sequence %d → %d — it was published", first, got)
	}
	if n := drain(sub); n != 0 {
		t.Errorf("subscribers received %d frames for an unchanged index, want 0", n)
	}
}

// TestNoteIndexChangePublishes checks the fence is a filter and not a wall:
// every kind of material change has to get through, or a note edit would never
// reach a client.
func TestNoteIndexChangePublishes(t *testing.T) {
	cases := map[string]func(map[string]*models.NoteIndexEntry){
		"title edited": func(m map[string]*models.NoteIndexEntry) { m["/nb/notes/007.md"].Title = "renamed" },
		"tag added": func(m map[string]*models.NoteIndexEntry) {
			m["/nb/notes/007.md"].Tags = []string{"alpha", "beta", "gamma"}
		},
		"mtime moved":   func(m map[string]*models.NoteIndexEntry) { m["/nb/notes/007.md"].ModTime = time.Unix(1, 0) },
		"priority set":  func(m map[string]*models.NoteIndexEntry) { m["/nb/notes/007.md"].Priority = "p0" },
		"group moved":   func(m map[string]*models.NoteIndexEntry) { m["/nb/notes/007.md"].Group = "plans/x" },
		"entry deleted": func(m map[string]*models.NoteIndexEntry) { delete(m, "/nb/notes/007.md") },
		"entry added": func(m map[string]*models.NoteIndexEntry) {
			m["/nb/notes/999.md"] = &models.NoteIndexEntry{Path: "/nb/notes/999.md"}
		},
		"whole index new": func(m map[string]*models.NoteIndexEntry) { clear(m) },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := New()
			s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: noteIndexFixture(20)})
			before := s.CurrentSeq()

			next := noteIndexFixture(20)
			mutate(next)
			s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: next})

			if s.CurrentSeq() == before {
				t.Fatal("a changed index was suppressed — the change would never reach a client")
			}
			if got := len(s.Get().NoteIndex); got != len(next) {
				t.Errorf("state holds %d entries, want %d", got, len(next))
			}
		})
	}
}

// TestNoteIndexFirstPublishAlwaysLands guards the seen flag: an empty index is
// a legitimate state (a host with no notebooks), and it must be applied and
// published rather than mistaken for "nothing changed yet".
func TestNoteIndexFirstPublishAlwaysLands(t *testing.T) {
	s := New()
	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: map[string]*models.NoteIndexEntry{}})
	if s.CurrentSeq() == 0 {
		t.Fatal("the first note index was suppressed")
	}
}

// TestNoteIndexDigestIsOrderIndependent pins the hash's one non-obvious
// property: it walks a map, so it must not depend on iteration order, and it
// must still separate a subset from the whole.
func TestNoteIndexDigestIsOrderIndependent(t *testing.T) {
	a, b := noteIndexFixture(200), noteIndexFixture(200)
	if noteIndexDigestOf(a) != noteIndexDigestOf(b) {
		t.Error("two identical indexes digested differently — map order leaked into the hash")
	}
	delete(b, "/nb/notes/010.md")
	if noteIndexDigestOf(a) == noteIndexDigestOf(b) {
		t.Error("a subset digested the same as the whole")
	}
}

// drain reports how many updates are queued on a subscription and empties it.
func drain(ch chan Update) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}
