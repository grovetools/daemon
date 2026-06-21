package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/workflowmon"
)

// fakeEventSource is a hand-fed workflowmon.EventSource.
type fakeEventSource struct {
	ch        chan workflowmon.Event
	closeOnce sync.Once
}

func newFakeEventSource() *fakeEventSource {
	return &fakeEventSource{ch: make(chan workflowmon.Event, 16)}
}

func (f *fakeEventSource) Events() <-chan workflowmon.Event { return f.ch }
func (f *fakeEventSource) Close() error {
	f.closeOnce.Do(func() { close(f.ch) })
	return nil
}

func TestWorkflowCollectorConvertsJournalEvents(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	st := store.New()

	// Confirmed claude session.
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessionIntent,
		Source:  "test",
		Payload: &store.SessionIntentPayload{JobID: "job-1", Provider: "claude"},
	})
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessionConfirmation,
		Source:  "test",
		Payload: &store.SessionConfirmationPayload{JobID: "job-1", NativeID: "sess-1", PID: 4242},
	})
	// A codex session and an unconfirmed claude session must be skipped.
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessionIntent,
		Source:  "test",
		Payload: &store.SessionIntentPayload{JobID: "job-codex", Provider: "codex"},
	})
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessionConfirmation,
		Source:  "test",
		Payload: &store.SessionConfirmationPayload{JobID: "job-codex", NativeID: "sess-codex", PID: 4243},
	})
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessionIntent,
		Source:  "test",
		Payload: &store.SessionIntentPayload{JobID: "job-pending", Provider: "claude"},
	})

	src := newFakeEventSource()
	var mu sync.Mutex
	spawned := make(map[string][]string) // claudeSessionID → dirs

	c := NewWorkflowCollector(10 * time.Millisecond)
	c.resolveDirs = func(claudeSessionID string) ([]string, error) {
		if claudeSessionID == "sess-1" {
			return []string{"/fake/slug-a/sess-1", "/fake/slug-b/sess-1"}, nil
		}
		return nil, nil
	}
	c.newSource = func(sessionDir string, opts workflowmon.FileSourceOptions) workflowmon.EventSource {
		mu.Lock()
		spawned[sessionDir] = opts.ScriptsDirs
		mu.Unlock()
		if sessionDir == "/fake/slug-a/sess-1" {
			return src
		}
		return newFakeEventSource()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx, st, updates)
	}()

	// Wait for the tailer to spawn, then feed journal events.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(spawned)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("tailers not spawned, got %v", spawned)
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Both resolved dirs' scripts dirs must be passed to every source
	// (cross-slug script fragmentation).
	mu.Lock()
	if got := spawned["/fake/slug-a/sess-1"]; len(got) != 2 {
		t.Errorf("ScriptsDirs = %v, want both slug dirs", got)
	}
	mu.Unlock()

	src.ch <- workflowmon.RunDiscovered{RunID: "wf_1", Meta: &workflowmon.ScriptMeta{
		Name:   "probe-flow",
		Phases: []workflowmon.PhaseMeta{{Title: "Phase 1"}},
	}}
	src.ch <- workflowmon.AgentStarted{RunID: "wf_1", AgentID: "a1", Name: "inspect:core", Prompt: "do it"}
	src.ch <- workflowmon.AgentCompleted{RunID: "wf_1", AgentID: "a1", Result: "ok"}
	src.ch <- workflowmon.RunStale{RunID: "wf_1"}

	wantTypes := []store.UpdateType{
		store.UpdateWorkflowRunDiscovered,
		store.UpdateWorkflowAgentStarted,
		store.UpdateWorkflowAgentCompleted,
		store.UpdateWorkflowRunStale,
	}
	for i, want := range wantTypes {
		select {
		case u := <-updates:
			if u.Type != want {
				t.Fatalf("update %d type = %q, want %q", i, u.Type, want)
			}
			payload, ok := u.Payload.(*store.WorkflowEventPayload)
			if !ok {
				t.Fatalf("update %d payload type %T", i, u.Payload)
			}
			ev := payload.Event
			if ev.JobID != "job-1" || ev.ClaudeSessionID != "sess-1" {
				t.Errorf("update %d attribution = %q/%q", i, ev.JobID, ev.ClaudeSessionID)
			}
			if ev.Source != models.WorkflowSourceJournal {
				t.Errorf("update %d source = %q, want journal", i, ev.Source)
			}
			if ev.Timestamp.IsZero() {
				t.Errorf("update %d missing receive timestamp", i)
			}
			switch want {
			case store.UpdateWorkflowRunDiscovered:
				if payload.RunName != "probe-flow" || len(payload.Phases) != 1 {
					t.Errorf("run meta = %q/%v", payload.RunName, payload.Phases)
				}
			case store.UpdateWorkflowAgentStarted:
				// The collector must propagate Name (the human label) along
				// with AgentID/Prompt — without it the store fold is starved
				// and rows lead with the raw agent id.
				if ev.AgentID != "a1" || ev.Prompt != "do it" || ev.Name != "inspect:core" {
					t.Errorf("started event = %+v", ev)
				}
			case store.UpdateWorkflowAgentCompleted:
				if ev.ResultSummary != "ok" {
					t.Errorf("completed event result = %q", ev.ResultSummary)
				}
			case store.UpdateWorkflowRunStale:
				if ev.RunID != "wf_1" {
					t.Errorf("stale event run = %q", ev.RunID)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for update %d (%s)", i, want)
		}
	}

	// Only the claude session got tailers.
	mu.Lock()
	for dir := range spawned {
		if dir != "/fake/slug-a/sess-1" && dir != "/fake/slug-b/sess-1" {
			t.Errorf("unexpected tailer for %s", dir)
		}
	}
	mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collector did not shut down")
	}
}
