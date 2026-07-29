package store

import (
	"fmt"
	"testing"
)

// publish drives an update through the real broadcast path so the test
// exercises the same sequencing the daemon uses.
func publishNote(t *testing.T, s *Store, tag string) {
	t.Helper()
	s.BroadcastConfigReload(tag)
}

func TestSequenceNumbersAreMonotonicFromOne(t *testing.T) {
	s := New()
	if got := s.CurrentSeq(); got != 0 {
		t.Fatalf("fresh store CurrentSeq = %d, want 0", got)
	}

	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	for i := 0; i < 3; i++ {
		publishNote(t, s, fmt.Sprintf("file-%d", i))
	}

	for want := uint64(1); want <= 3; want++ {
		u := <-ch
		if u.Seq != want {
			t.Fatalf("update %d carried Seq %d, want %d", want, u.Seq, want)
		}
	}
	if got := s.CurrentSeq(); got != 3 {
		t.Fatalf("CurrentSeq = %d, want 3", got)
	}
}

// Every broadcast entry point must route through publishLocked, or a ?since=
// replay would silently skip whatever it minted.
func TestAllBroadcastPathsAreSequenced(t *testing.T) {
	s := New()
	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	s.BroadcastConfigReload("grove.toml")
	s.BroadcastBootPhase(struct{ Done bool }{Done: true})
	s.SetFocus("test", []string{t.TempDir()})
	s.ApplyUpdate(Update{Type: UpdateWatcherStatus, Source: "test", Payload: "up"})

	seen := make(map[UpdateType]uint64)
	for i := 0; i < 4; i++ {
		u := <-ch
		if u.Seq == 0 {
			t.Fatalf("update %s reached a subscriber with Seq 0 — it bypassed publishLocked", u.Type)
		}
		seen[u.Type] = u.Seq
	}
	for _, want := range []UpdateType{UpdateConfigReload, UpdateBootPhase, UpdateFocus, UpdateWatcherStatus} {
		if seen[want] == 0 {
			t.Errorf("no sequenced update of type %s", want)
		}
	}
}

func TestReplayReturnsEverythingAfterCursor(t *testing.T) {
	s := New()
	for i := 0; i < 5; i++ {
		publishNote(t, s, fmt.Sprintf("file-%d", i))
	}

	got, gap := s.Replay(2)
	if gap.Gapped() {
		t.Fatalf("replay from 2 with a 5-entry history reported a gap: %+v", gap)
	}
	if len(got) != 3 {
		t.Fatalf("replayed %d updates, want 3", len(got))
	}
	for i, u := range got {
		if want := uint64(3 + i); u.Seq != want {
			t.Errorf("replay[%d].Seq = %d, want %d", i, u.Seq, want)
		}
	}
	if gap.Current != 5 || gap.Oldest != 1 {
		t.Errorf("gap bookkeeping = %+v, want Current 5 / Oldest 1", gap)
	}
}

func TestReplayFromCurrentIsEmptyAndUngapped(t *testing.T) {
	s := New()
	publishNote(t, s, "a")

	got, gap := s.Replay(1)
	if len(got) != 0 {
		t.Fatalf("replay from the head returned %d updates, want 0", len(got))
	}
	if gap.Gapped() {
		t.Fatalf("replay from the head reported a gap: %+v", gap)
	}
}

func TestReplayFromZeroOnAnEmptyBusIsUngapped(t *testing.T) {
	s := New()
	got, gap := s.Replay(0)
	if len(got) != 0 || gap.Gapped() {
		t.Fatalf("empty bus replay = %d updates, gap %+v; want 0 and no gap", len(got), gap)
	}
}

func TestReplayReportsTooOldOnceTheRingWraps(t *testing.T) {
	s := New()
	for i := 0; i < RingSize+10; i++ {
		publishNote(t, s, "x")
	}

	got, gap := s.Replay(1)
	if gap.Reason != ReplayGapTooOld {
		t.Fatalf("gap reason = %q, want %q", gap.Reason, ReplayGapTooOld)
	}
	if gap.Oldest != 11 {
		t.Errorf("gap.Oldest = %d, want 11 (%d published, ring holds %d)", gap.Oldest, RingSize+10, RingSize)
	}
	// A too-old replay still hands back what IS retained, so a caller that
	// prefers a partial tail to nothing can use it.
	if len(got) != RingSize {
		t.Errorf("too-old replay returned %d updates, want the full retained ring (%d)", len(got), RingSize)
	}
	if got[0].Seq != 11 {
		t.Errorf("oldest retained Seq = %d, want 11", got[0].Seq)
	}
}

// A cursor ahead of the daemon means the daemon restarted: sequences restart
// at 1, so the client's cursor is from a previous incarnation.
func TestReplayReportsResetForAFutureCursor(t *testing.T) {
	s := New()
	publishNote(t, s, "a")

	got, gap := s.Replay(9999)
	if gap.Reason != ReplayGapReset {
		t.Fatalf("gap reason = %q, want %q", gap.Reason, ReplayGapReset)
	}
	if len(got) != 0 {
		t.Fatalf("a reset replay returned %d updates, want 0", len(got))
	}
	if gap.Current != 1 || gap.Since != 9999 {
		t.Errorf("gap = %+v, want Current 1 / Since 9999", gap)
	}
}

// The ring must keep recording even when nobody is subscribed — that is the
// whole point of a replay cursor across a disconnect.
func TestRingRecordsWhileNoSubscribersExist(t *testing.T) {
	s := New()
	publishNote(t, s, "before")
	publishNote(t, s, "during")

	got, gap := s.Replay(0)
	if gap.Gapped() {
		t.Fatalf("unexpected gap: %+v", gap)
	}
	if len(got) != 2 {
		t.Fatalf("replayed %d updates with no subscribers attached, want 2", len(got))
	}
}
