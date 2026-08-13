package store

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/grovetools/core/pkg/models"
)

// bigNoteIndex builds one note-index generation shaped like the real one: an
// absolute path per entry plus the handful of short strings the enrichment
// indexer fills in. gen varies the content so successive generations are
// distinct (the digest fence would otherwise suppress them) and share no
// strings (the real indexer allocates fresh strings every rebuild).
func bigNoteIndex(gen, n int) map[string]*models.NoteIndexEntry {
	index := make(map[string]*models.NoteIndexEntry, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("/Users/x/notebooks/grovetools/notespaces/ws%03d/plans/plan-%04d/.artifacts/job-%05d-gen%d.md", i%700, i%400, i, gen)
		index[p] = &models.NoteIndexEntry{
			Path:       p,
			Name:       fmt.Sprintf("job-%05d-gen%d.md", i, gen),
			Title:      fmt.Sprintf("Job %05d generation %d", i, gen),
			Type:       "note",
			Group:      fmt.Sprintf("plans/plan-%04d/.artifacts", i%400),
			Workspace:  fmt.Sprintf("ws%03d", i%700),
			ContentDir: "plans",
		}
	}
	return index
}

// TestRingDoesNotAccumulateNoteIndexGenerations is the regression fence for the
// dominant retainer found on the 2026-08-13 heap profile.
//
// The note collector rebuilds the WHOLE index every 5 minutes and republishes
// it. Each generation is a fresh map of fresh entries, so the ring — which held
// published updates by reference — pinned every superseded generation until
// 1024 further updates evicted it. On the live daemon that was ~12 generations
// of a 38k-entry index: 244 MB, 38% of the post-GC live heap, unreachable by
// any consumer (see RingDropsPayload).
//
// The assertion is deliberately loose (a factor, not a byte count) because it
// only needs to separate "retains one generation" from "retains all of them";
// without the fix this measured ~generations× the per-generation cost.
func TestRingDoesNotAccumulateNoteIndexGenerations(t *testing.T) {
	const (
		entries     = 20000
		generations = 10
	)

	s := New()

	// Publish one generation first and measure the floor WITH it resident, so
	// the baseline already contains everything a correctly-bounded store keeps.
	s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: bigNoteIndex(0, entries)})
	runtime.GC()
	runtime.GC()
	var floor runtime.MemStats
	runtime.ReadMemStats(&floor)

	for gen := 1; gen <= generations; gen++ {
		s.ApplyUpdate(Update{Type: UpdateNoteIndex, Source: "note", Payload: bigNoteIndex(gen, entries)})
	}
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Sanity: every generation differed, so the digest fence let them all
	// through and the ring really did see `generations` publishes.
	replay, _ := s.Replay(0)
	if len(replay) != generations+1 {
		t.Fatalf("ring recorded %d note-index updates, want %d — the fence suppressed publishes and this test proves nothing",
			len(replay), generations+1)
	}

	growth := int64(after.HeapAlloc) - int64(floor.HeapAlloc)
	perGeneration := int64(floor.HeapAlloc) / 2 // conservative lower bound on one generation's cost
	t.Logf("resident after 1 generation: %.1f MB", float64(floor.HeapAlloc)/(1<<20))
	t.Logf("growth over %d further generations: %.1f MB", generations, float64(growth)/(1<<20))

	// Replacing an index must cost about ONE generation, not `generations` of
	// them. Allow generous slack for allocator noise and the surviving newest
	// map; fail well below the "every generation retained" signal.
	if growth > 2*perGeneration {
		t.Errorf("replay ring grew %.1f MB across %d note-index replacements (one generation is ~%.1f MB) — "+
			"superseded generations are being retained again",
			float64(growth)/(1<<20), generations, float64(floor.HeapAlloc)/(1<<20))
	}
}
