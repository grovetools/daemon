package collector

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/workflowmon"
)

// WorkflowCollector tails Claude Code workflow journals for every confirmed
// claude session (ClaudeSessionID set — the intent→confirm handshake
// brackets every verified agent run) and converts journal events into
// workflow store updates with Source="journal". One tailer set per host
// instead of one per TUI.
//
// Division of authority (see the workflow-subagent integration spec): the
// journal supplies run-ID attribution, prompts, and structured results that
// hooks lack; hooks supply wall-clock timestamps and transcript paths the
// journal lacks. The store's fold dedupes the two streams by
// (RunID, AgentID).
//
// Liveness: run staleness comes exclusively from the workflowmon.FileSource
// heuristic (quiet journal + dead session, via the store's view of session
// status) — never from the PID reaper: workflow runs outlive hook PIDs and
// Mux=none headless agents may have PID 0.
type WorkflowCollector struct {
	interval time.Duration
	ulog     *logging.UnifiedLogger

	// Test seams.
	resolveDirs func(claudeSessionID string) ([]string, error)
	newSource   func(sessionDir string, opts workflowmon.FileSourceOptions) workflowmon.EventSource
}

// sessionTailers tracks the running FileSources for one session, keyed by
// session dir (artifacts fragment across project-slug dirs, so a session
// can grow new dirs mid-run).
type sessionTailers struct {
	jobID           string
	claudeSessionID string
	sources         map[string]workflowmon.EventSource
}

// NewWorkflowCollector creates a workflow journal watcher. Defaults to a 5s
// session-discovery interval (each FileSource polls its own dirs at 1s).
func NewWorkflowCollector(interval time.Duration) *WorkflowCollector {
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &WorkflowCollector{
		interval:    interval,
		ulog:        logging.NewUnifiedLogger("groved.collector.workflow"),
		resolveDirs: sessions.ResolveClaudeSessionDirs,
		newSource: func(sessionDir string, opts workflowmon.FileSourceOptions) workflowmon.EventSource {
			return workflowmon.NewFileSource(sessionDir, opts)
		},
	}
}

// Name returns the collector's name.
func (c *WorkflowCollector) Name() string { return "workflow" }

// Run starts the session-discovery loop, spawning journal tailers for
// confirmed claude sessions and tearing them down when sessions vanish
// from the store.
func (c *WorkflowCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	tracked := make(map[string]*sessionTailers) // keyed by session (job) ID
	var wg sync.WaitGroup

	defer func() {
		for _, t := range tracked {
			for _, src := range t.sources {
				_ = src.Close()
			}
		}
		wg.Wait()
	}()

	c.ulog.Info("Workflow journal collector started").Log(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.reconcile(ctx, st, updates, tracked, &wg)
		}
	}
}

// reconcile diffs the store's sessions against the tracked tailers:
// spawning sources for new confirmed claude sessions (and for session dirs
// that appeared later via slug fragmentation), and closing tailers whose
// session left the store entirely. Ended-but-present sessions keep their
// tailers so the FileSource staleness heuristic can observe the dead
// session and emit RunStale.
func (c *WorkflowCollector) reconcile(ctx context.Context, st *store.Store, updates chan<- store.Update, tracked map[string]*sessionTailers, wg *sync.WaitGroup) {
	current := make(map[string]*models.Session)
	for _, sess := range st.GetSessions() {
		// Workflow internals are Claude-provider-only: codex/opencode (and
		// oneshot/chat jobs) have no journal source and degrade to no
		// workflow data. An empty provider with a ClaudeSessionID set is
		// treated as claude — the session ID only exists for claude runs.
		if sess.ClaudeSessionID == "" || !isClaudeProvider(sess.Provider) {
			continue
		}
		current[sess.ID] = sess
	}

	// Drop tailers for sessions gone from the store.
	for id, t := range tracked {
		if _, ok := current[id]; !ok {
			for _, src := range t.sources {
				_ = src.Close()
			}
			delete(tracked, id)
		}
	}

	for id, sess := range current {
		t, ok := tracked[id]
		if !ok {
			t = &sessionTailers{
				jobID:           sess.ID,
				claudeSessionID: sess.ClaudeSessionID,
				sources:         make(map[string]workflowmon.EventSource),
			}
			tracked[id] = t
		}

		dirs, err := c.resolveDirs(sess.ClaudeSessionID)
		if err != nil || len(dirs) == 0 {
			continue
		}

		// Merge every resolved dir's scripts dir: a run discovered under
		// one project slug may have its script persisted under another.
		scriptsDirs := make([]string, 0, len(dirs))
		for _, dir := range dirs {
			scriptsDirs = append(scriptsDirs, filepath.Join(dir, "workflows", "scripts"))
		}

		for _, dir := range dirs {
			if _, ok := t.sources[dir]; ok {
				continue
			}
			jobID := t.jobID
			src := c.newSource(dir, workflowmon.FileSourceOptions{
				ScriptsDirs: scriptsDirs,
				// Session liveness from the store's session state — NOT a
				// PID probe. Staleness requires BOTH a quiet journal and a
				// gone session (the workflowmon heuristic).
				SessionAlive: func() bool {
					sess := st.GetSession(jobID)
					if sess == nil {
						return false
					}
					switch sess.Status {
					case "completed", "interrupted", "failed":
						return false
					}
					return true
				},
			})
			t.sources[dir] = src
			c.ulog.Debug("Tailing workflow journals").
				Field("job_id", t.jobID).
				Field("session_dir", dir).
				Log(ctx)

			wg.Add(1)
			go func(t *sessionTailers, src workflowmon.EventSource) {
				defer wg.Done()
				c.pump(ctx, t, src, updates)
			}(t, src)
		}
	}
}

// pump converts one FileSource's events into store updates until the source
// closes or the context ends.
func (c *WorkflowCollector) pump(ctx context.Context, t *sessionTailers, src workflowmon.EventSource, updates chan<- store.Update) {
	for ev := range src.Events() {
		upd, ok := c.convertEvent(t.jobID, t.claudeSessionID, ev)
		if !ok {
			continue
		}
		select {
		case updates <- upd:
		case <-ctx.Done():
			return
		}
	}
}

// convertEvent maps a workflowmon event to a workflow store update with
// Source="journal". The journal carries no timestamps, so events are
// stamped with daemon receive time (the store's merge gives hook-supplied
// timestamps precedence).
func (c *WorkflowCollector) convertEvent(jobID, claudeSessionID string, ev workflowmon.Event) (store.Update, bool) {
	base := models.WorkflowEvent{
		JobID:           jobID,
		ClaudeSessionID: claudeSessionID,
		Timestamp:       time.Now(),
		Source:          models.WorkflowSourceJournal,
	}

	switch e := ev.(type) {
	case workflowmon.RunDiscovered:
		base.Kind = models.WorkflowRunDiscovered
		base.RunID = e.RunID
		payload := &store.WorkflowEventPayload{Event: base}
		if e.Meta != nil {
			payload.RunName = e.Meta.Name
			for _, phase := range e.Meta.Phases {
				payload.Phases = append(payload.Phases, phase.Title)
			}
		}
		return store.Update{Type: store.UpdateWorkflowRunDiscovered, Source: c.Name(), Payload: payload}, true

	case workflowmon.AgentStarted:
		base.Kind = models.WorkflowAgentStarted
		base.RunID = e.RunID
		base.AgentID = e.AgentID
		base.Name = e.Name
		base.Prompt = e.Prompt
		base.Phase = e.Phase
		return store.Update{Type: store.UpdateWorkflowAgentStarted, Source: c.Name(), Payload: &store.WorkflowEventPayload{Event: base}}, true

	case workflowmon.AgentCompleted:
		base.Kind = models.WorkflowAgentCompleted
		base.RunID = e.RunID
		base.AgentID = e.AgentID
		base.ResultSummary = e.Result
		return store.Update{Type: store.UpdateWorkflowAgentCompleted, Source: c.Name(), Payload: &store.WorkflowEventPayload{Event: base}}, true

	case workflowmon.RunStale:
		base.Kind = models.WorkflowRunStale
		base.RunID = e.RunID
		return store.Update{Type: store.UpdateWorkflowRunStale, Source: c.Name(), Payload: &store.WorkflowEventPayload{Event: base}}, true
	}

	return store.Update{}, false
}

// isClaudeProvider reports whether a session provider produces Claude Code
// session artifacts. Empty is allowed: only claude runs ever get a
// ClaudeSessionID, and older intents may omit the provider.
func isClaudeProvider(provider string) bool {
	return provider == "" || strings.HasPrefix(provider, "claude")
}
