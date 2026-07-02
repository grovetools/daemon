package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// writeTokenTranscript writes a single-message assistant transcript line to
// path, creating parent dirs. The token counts drive the summarizer.
func writeTokenTranscript(t *testing.T, path, sessionID string, input, output int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"` + sessionID +
		`","requestId":"r1","timestamp":"2026-01-01T00:00:00.000Z","message":{"id":"m1",` +
		`"model":"claude-opus-4-5","usage":{"input_tokens":` + itoaTok(input) +
		`,"output_tokens":` + itoaTok(output) + `,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoaTok(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestRefreshLiveTokens_PopulatesFields is the end-to-end unit for Part 1:
// a live agent session with a small transcript fixture yields a
// UpdateSessionTokens whose fields (LiveTokens, LiveCostUSD, ContextSize) are
// populated, and applying it stamps the store's models.Session.
func TestRefreshLiveTokens_PopulatesFields(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	sid := "agent-session-1"
	slug := filepath.Join(root, "projects", "-Users-me-proj")
	transcript := filepath.Join(slug, sid+".jsonl")
	writeTokenTranscript(t, transcript, sid, 1500, 250)

	sess := &models.Session{
		ID:              "job-1",
		Status:          "running",
		ClaudeSessionID: sid,
		TranscriptPath:  transcript,
	}

	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessions,
		Source:  "test",
		Payload: []*models.Session{sess},
	})

	c := NewSessionCollector(2*time.Second, "")

	updates := make(chan store.Update, 4)
	c.refreshLiveTokens(context.Background(), []*models.Session{sess}, updates)

	select {
	case u := <-updates:
		if u.Type != store.UpdateSessionTokens {
			t.Fatalf("update type = %q, want session_tokens", u.Type)
		}
		payload, ok := u.Payload.(*store.SessionTokensPayload)
		if !ok || len(payload.Updates) != 1 {
			t.Fatalf("payload = %#v, want 1 SessionTokenUpdate", u.Payload)
		}
		tu := payload.Updates[0]
		if tu.JobID != "job-1" {
			t.Errorf("JobID = %q, want job-1", tu.JobID)
		}
		if tu.LiveTokens <= 0 {
			t.Errorf("LiveTokens = %d, want > 0", tu.LiveTokens)
		}
		if tu.LiveCostUSD <= 0 {
			t.Errorf("LiveCostUSD = %g, want > 0", tu.LiveCostUSD)
		}
		if tu.ContextSize <= 0 {
			t.Errorf("ContextSize = %d, want > 0", tu.ContextSize)
		}

		// Applying the update stamps the store record — the path treemux reads
		// via /api/sessions.
		st.ApplyUpdate(u)
		got := st.GetSession("job-1")
		if got == nil {
			t.Fatal("session job-1 missing from store")
		}
		if got.LiveTokens != tu.LiveTokens || got.LiveCostUSD != tu.LiveCostUSD || got.ContextSize != tu.ContextSize {
			t.Errorf("store session token fields = (%d, %g, %d), want (%d, %g, %d)",
				got.LiveTokens, got.LiveCostUSD, got.ContextSize,
				tu.LiveTokens, tu.LiveCostUSD, tu.ContextSize)
		}
	default:
		t.Fatal("refreshLiveTokens emitted no update for a live agent with tokens")
	}
}

// TestRefreshLiveTokens_MtimeSkip verifies the cost control: a second refresh
// over an unchanged transcript re-parses nothing and emits no update.
func TestRefreshLiveTokens_MtimeSkip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	sid := "agent-session-2"
	slug := filepath.Join(root, "projects", "-Users-me-proj2")
	transcript := filepath.Join(slug, sid+".jsonl")
	writeTokenTranscript(t, transcript, sid, 800, 120)

	sess := &models.Session{ID: "job-2", Status: "running", ClaudeSessionID: sid, TranscriptPath: transcript}
	c := NewSessionCollector(2*time.Second, "")

	updates := make(chan store.Update, 4)
	// First pass emits an update (new data).
	c.refreshLiveTokens(context.Background(), []*models.Session{sess}, updates)
	if len(updates) != 1 {
		t.Fatalf("first pass emitted %d updates, want 1", len(updates))
	}
	<-updates

	// Second pass over the unchanged transcript must skip (mtime unchanged).
	c.refreshLiveTokens(context.Background(), []*models.Session{sess}, updates)
	if len(updates) != 0 {
		t.Fatalf("second pass emitted %d updates over unchanged transcript, want 0", len(updates))
	}
}

// TestRefreshLiveTokens_SkipsNonAgents verifies shells and completed sessions
// are never summarized.
func TestRefreshLiveTokens_SkipsNonAgents(t *testing.T) {
	c := NewSessionCollector(2*time.Second, "")
	updates := make(chan store.Update, 4)

	shell := &models.Session{ID: "shell", Status: "running"} // no ClaudeSessionID
	done := &models.Session{ID: "done", Status: "completed", ClaudeSessionID: "x", TranscriptPath: "/nope"}
	c.refreshLiveTokens(context.Background(), []*models.Session{shell, done}, updates)

	if len(updates) != 0 {
		t.Fatalf("emitted %d updates for non-agent/completed sessions, want 0", len(updates))
	}
	if isLiveAgentSession(shell) {
		t.Error("shell (no ClaudeSessionID) classified as live agent")
	}
	if isLiveAgentSession(done) {
		t.Error("completed session classified as live agent")
	}
}
