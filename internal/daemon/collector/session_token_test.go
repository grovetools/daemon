package collector

import (
	"context"
	"errors"
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

// TestIsLiveAgentSession_NonClaude verifies the relaxed gate: a non-claude
// provider qualifies a session even without a ClaudeSessionID (opencode
// registers with neither an ID nor a transcript path), while provider-less
// shells stay excluded.
func TestIsLiveAgentSession_NonClaude(t *testing.T) {
	cases := []struct {
		name string
		s    *models.Session
		want bool
	}{
		{"opencode running, no ids", &models.Session{Provider: "opencode", Status: "running"}, true},
		{"codex running, no ids", &models.Session{Provider: "codex", Status: "running"}, true},
		{"plain shell, all empty", &models.Session{Status: "running"}, false},
		{"claude provider, no session id", &models.Session{Provider: "claude", Status: "running"}, false},
		{"transcript path only", &models.Session{TranscriptPath: "/t.jsonl", Status: "running"}, true},
		{"non-claude but completed", &models.Session{Provider: "codex", Status: "completed"}, false},
		{"nil session", nil, false},
	}
	for _, tc := range cases {
		if got := isLiveAgentSession(tc.s); got != tc.want {
			t.Errorf("%s: isLiveAgentSession = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// writeCodexTranscript writes a minimal codex rollout fixture: session_meta,
// a turn_context naming the model, and one cumulative token_count event.
func writeCodexTranscript(t *testing.T, path, model string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"timestamp":"2026-07-01T10:00:00.000Z","type":"session_meta","payload":{"id":"5973b6c0-94b8-487b-a530-2aeb6098ae0e","cwd":"/Users/test/project"}}
{"timestamp":"2026-07-01T10:00:01.000Z","type":"turn_context","payload":{"model":"` + model + `"}}
{"timestamp":"2026-07-01T10:00:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":1000,"output_tokens":150,"reasoning_output_tokens":40,"total_tokens":1350},"last_token_usage":{"input_tokens":1200,"cached_input_tokens":1000,"output_tokens":150,"reasoning_output_tokens":40,"total_tokens":1350},"model_context_window":272000},"rate_limits":null}}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSummarizeLiveSession_ProviderBranch verifies the collector's provider
// branch: claude sessions summarize via slug-dir discovery, non-claude
// sessions via the provider-routed single-transcript summarizer — and the
// extracted model rides the SessionTokenUpdate into the store.
func TestTranscriptResolutionBackoffBecomesPermanentAndResets(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	c := NewSessionCollector(2*time.Second, "")
	c.now = func() time.Time { return now }
	c.resolveTranscript = func(string) (string, string, error) {
		calls++
		return "", "", os.ErrNotExist
	}
	s := &models.Session{ID: "missing-pi", Provider: "pi", Status: "running", PlanDirectory: "/plan/a"}

	for failure := 1; failure <= transcriptResolveMaxFailures; failure++ {
		_, _, err := c.summarizeLiveSession(s)
		if err == nil || calls != failure {
			t.Fatalf("failure %d: err=%v calls=%d", failure, err, calls)
		}
		_, _, retryErr := c.summarizeLiveSession(s)
		if failure == transcriptResolveMaxFailures {
			if !errors.Is(retryErr, errResolvePermanent) {
				t.Fatalf("after max failures err=%v, want permanent", retryErr)
			}
		} else {
			if !errors.Is(retryErr, errResolveThrottled) {
				t.Fatalf("failure %d immediate retry err=%v, want throttled", failure, retryErr)
			}
			now = now.Add(transcriptResolveBackoff(failure))
		}
		if calls != failure {
			t.Fatalf("wait-state retry called resolver: calls=%d want=%d", calls, failure)
		}
	}

	// New registration metadata for the same job clears permanent failure and
	// permits an immediate fresh attempt.
	s.PlanDirectory = "/plan/b"
	_, _, _ = c.summarizeLiveSession(s)
	if calls != transcriptResolveMaxFailures+1 {
		t.Fatalf("registration change did not reset resolution: calls=%d", calls)
	}
}

func TestTranscriptResolveBackoffCaps(t *testing.T) {
	if got := transcriptResolveBackoff(1); got != 30*time.Second {
		t.Fatalf("first backoff=%s", got)
	}
	if got := transcriptResolveBackoff(99); got != transcriptResolveMaxBackoff {
		t.Fatalf("capped backoff=%s, want %s", got, transcriptResolveMaxBackoff)
	}
}

func TestSummarizeLiveSession_ProviderBranch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	c := NewSessionCollector(2*time.Second, "")

	// Claude branch: model comes from the transcript's assistant line.
	sid := "agent-session-3"
	claudeTranscript := filepath.Join(root, "projects", "-Users-me-proj3", sid+".jsonl")
	writeTokenTranscript(t, claudeTranscript, sid, 500, 80)
	claudeSess := &models.Session{ID: "job-claude", Status: "running", ClaudeSessionID: sid, TranscriptPath: claudeTranscript}

	summary, usedPath, err := c.summarizeLiveSession(claudeSess)
	if err != nil {
		t.Fatalf("summarizeLiveSession(claude): %v", err)
	}
	if usedPath != claudeTranscript {
		t.Errorf("claude usedPath = %q, want %q", usedPath, claudeTranscript)
	}
	if len(summary.ModelBreakdown) == 0 || summary.ModelBreakdown[0].Model != "claude-opus-4-5" {
		t.Errorf("claude ModelBreakdown = %+v, want [0].Model = claude-opus-4-5", summary.ModelBreakdown)
	}

	// Codex branch: provider-routed transcript summarizer, model from
	// turn_context.
	codexTranscript := filepath.Join(root, "codex-sessions", "rollout-2026-07-01T10-00-00-5973b6c0-94b8-487b-a530-2aeb6098ae0e.jsonl")
	writeCodexTranscript(t, codexTranscript, "gpt-5.5")
	codexSess := &models.Session{ID: "job-codex", Status: "running", Provider: "codex", ClaudeSessionID: "native-codex-id", TranscriptPath: codexTranscript}

	summary, usedPath, err = c.summarizeLiveSession(codexSess)
	if err != nil {
		t.Fatalf("summarizeLiveSession(codex): %v", err)
	}
	if usedPath != codexTranscript {
		t.Errorf("codex usedPath = %q, want %q", usedPath, codexTranscript)
	}
	if len(summary.ModelBreakdown) == 0 || summary.ModelBreakdown[0].Model != "gpt-5.5" {
		t.Fatalf("codex ModelBreakdown = %+v, want [0].Model = gpt-5.5", summary.ModelBreakdown)
	}

	// End-to-end: refreshLiveTokens emits Model and the store applies it.
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSessions,
		Source:  "test",
		Payload: []*models.Session{codexSess},
	})

	updates := make(chan store.Update, 4)
	c.refreshLiveTokens(context.Background(), []*models.Session{codexSess}, updates)
	select {
	case u := <-updates:
		payload, ok := u.Payload.(*store.SessionTokensPayload)
		if !ok || len(payload.Updates) != 1 {
			t.Fatalf("payload = %#v, want 1 SessionTokenUpdate", u.Payload)
		}
		if payload.Updates[0].Model != "gpt-5.5" {
			t.Errorf("update Model = %q, want gpt-5.5", payload.Updates[0].Model)
		}
		st.ApplyUpdate(u)
		got := st.GetSession("job-codex")
		if got == nil {
			t.Fatal("session job-codex missing from store")
		}
		if got.Model != "gpt-5.5" {
			t.Errorf("store session Model = %q, want gpt-5.5", got.Model)
		}
	default:
		t.Fatal("refreshLiveTokens emitted no update for a live codex session")
	}
}
