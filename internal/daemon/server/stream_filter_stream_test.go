package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// shortSocketPath returns a unix socket path well inside the ~104-byte sun_path
// limit. t.TempDir() embeds the test name, which for these tests is long enough
// to overflow it and fail with a bare "bind: invalid argument".
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gsse")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// sseTap is one live /api/stream subscription under test. Frames are drained by
// a single goroutine into a channel: a per-call reader would keep reading after
// a timed-out next() and steal the frame the following call is waiting for.
type sseTap struct {
	frames chan coredaemon.StateUpdate
}

// subscribeSSE opens /api/stream with the given raw query and consumes the
// ": connected" preamble, which the handler writes only AFTER registering its
// store subscriber. Returning past it therefore means this tap will observe
// every subsequent broadcast — without that guarantee the test would race.
func subscribeSSE(t *testing.T, sock, query string) *sseTap {
	t.Helper()
	url := "http://unix/api/stream"
	if query != "" {
		url += "?" + query
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := unixHTTPClient(sock)
	client.Timeout = 0 // streaming — no whole-response deadline

	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("subscribe %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read SSE preamble: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("first SSE line = %q, want the ': connected' comment", line)
	}

	tap := &sseTap{frames: make(chan coredaemon.StateUpdate, 64)}
	go func() {
		defer close(tap.frames)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var u coredaemon.StateUpdate
			if err := json.Unmarshal([]byte(payload), &u); err != nil {
				continue
			}
			tap.frames <- u
		}
	}()
	return tap
}

// next returns the next decoded frame, or nil if none arrives within timeout.
func (tp *sseTap) next(t *testing.T, timeout time.Duration) *coredaemon.StateUpdate {
	t.Helper()
	select {
	case u, ok := <-tp.frames:
		if !ok {
			return nil
		}
		return &u
	case <-time.After(timeout):
		return nil
	}
}

// TestStreamFilterEndToEnd is the contract test for the whole mechanism: two
// subscribers on the same daemon, one unfiltered and one declaring the types
// flow's status view acts on, fed the same broadcasts.
//
// It is deliberately an HTTP-level test rather than a unit test of the matcher:
// the risk this change carries is not "does the predicate work" but "does an
// existing, unmodified subscriber still get everything", and only the real
// handler can answer that.
func TestStreamFilterEndToEnd(t *testing.T) {
	sock := shortSocketPath(t)
	st := store.New()
	// Give the snapshot something to carry, so "the filtered client skipped a
	// non-empty initial frame" is a real assertion.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/ws/alpha": {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/alpha"}},
			"/ws/beta":  {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/beta"}},
		},
	})

	s := New(false)
	s.SetEngine(engine.New(st))
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	filteredBefore := telemetry.SSEEventsFiltered.Value()
	skippedBefore := telemetry.SSEInitialSkipped.Value()

	unfiltered := subscribeSSE(t, sock, "")
	filtered := subscribeSSE(t, sock, "types=session,job_started")

	// (a) The unfiltered subscriber still gets the snapshot, with every
	//     workspace on it. This is the old-client guarantee.
	first := unfiltered.next(t, 3*time.Second)
	if first == nil {
		t.Fatal("unfiltered subscriber received no initial frame")
	}
	if first.UpdateType != coredaemon.StreamTypeInitial {
		t.Fatalf("unfiltered first frame = %q, want %q", first.UpdateType, coredaemon.StreamTypeInitial)
	}
	if len(first.Workspaces) != 2 {
		t.Fatalf("initial frame carried %d workspaces, want 2", len(first.Workspaces))
	}

	// Fan out one frame the filter rejects, then one it accepts. Sending the
	// rejected one FIRST is the point: if filtering were a no-op the filtered
	// tap's next frame would be note_index, not job_started.
	go func() {
		time.Sleep(150 * time.Millisecond)
		st.ApplyUpdate(store.Update{Type: store.UpdateNoteIndex, Source: "test"})
		st.ApplyUpdate(store.Update{Type: store.UpdateJobStarted, Source: "test"})
	}()

	// (b) The filtered subscriber's FIRST frame ever is the job event: no
	//     snapshot, no note_index.
	got := filtered.next(t, 4*time.Second)
	if got == nil {
		t.Fatal("filtered subscriber received nothing")
	}
	if got.UpdateType != "job_started" {
		t.Fatalf("filtered subscriber's first frame = %q, want job_started "+
			"(a snapshot or note_index here means the filter did not apply)", got.UpdateType)
	}

	// (c) The unfiltered subscriber saw the rejected frame too — the filter is
	//     per-subscription, not a global mute.
	sawNoteIndex := false
	for i := 0; i < 6 && !sawNoteIndex; i++ {
		u := unfiltered.next(t, 2*time.Second)
		if u == nil {
			break
		}
		if u.UpdateType == "note_index" {
			sawNoteIndex = true
		}
	}
	if !sawNoteIndex {
		t.Error("unfiltered subscriber never saw note_index — filtering leaked across subscriptions")
	}

	// (d) The counters back the claim the whole job is measured by.
	if delta := telemetry.SSEEventsFiltered.Value() - filteredBefore; delta < 1 {
		t.Errorf("sse.events.filtered moved by %d, want at least 1", delta)
	}
	if delta := telemetry.SSEInitialSkipped.Value() - skippedBefore; delta != 1 {
		t.Errorf("sse.initial.skipped moved by %d, want exactly 1", delta)
	}
}

// A subscriber that names "initial" keeps the snapshot: the type is an ordinary
// member of the allow-list, so a TUI that does read workspace state (or needs
// the theme/boot payload the snapshot carries) can opt back in.
func TestStreamFilterCanOptIntoSnapshot(t *testing.T) {
	sock := shortSocketPath(t)
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/ws/alpha": {WorkspaceNode: &workspace.WorkspaceNode{Path: "/ws/alpha"}},
		},
	})

	s := New(false)
	s.SetEngine(engine.New(st))
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	tap := subscribeSSE(t, sock, "types=initial,session")
	first := tap.next(t, 3*time.Second)
	if first == nil || first.UpdateType != coredaemon.StreamTypeInitial {
		t.Fatalf("first frame = %+v, want an initial snapshot", first)
	}
	if len(first.Workspaces) != 1 {
		t.Fatalf("snapshot carried %d workspaces, want 1", len(first.Workspaces))
	}
}
