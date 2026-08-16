package jobrunner

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
	tuimux "github.com/grovetools/tuimux/api/client"
	tuimuxpty "github.com/grovetools/tuimux/pty"
)

// agentPtyMeta builds a live-PTY metadata record the way the scoped tuimux
// daemon reports agent panes: type=agent plus a job_id tag.
func agentPtyMeta(ptyID, jobID string) tuimuxpty.SessionMetadata {
	tags := map[string]string{"type": "agent"}
	if jobID != "" {
		tags["job_id"] = jobID
	}
	return tuimuxpty.SessionMetadata{
		ID:        ptyID,
		CWD:       "/tmp/somewhere",
		Tags:      tags,
		StartedAt: time.Now(),
	}
}

// fakeTuimuxd serves GET /api/pty/list over a unix socket, exactly the endpoint
// listLivePtys dials. Returns the socket path. The socket lives in a short
// os.MkdirTemp dir, not t.TempDir(): long test names can push t.TempDir()
// past the macOS 104-byte sun_path limit.
func fakeTuimuxd(t *testing.T, metas []tuimuxpty.SessionMetadata) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tmx")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := dir + "/tuimux.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{ //nolint:gosec // G112: test server
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/pty/list" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(metas)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// The incident this guards against: a daemon with NO persisted jobs meets a
// tuimux daemon full of live agent PTYs owned by another groved. It must
// rebuild nothing — every session it imports here is a session the plain-stop
// shutdown path will KillPty.
func TestAdoptFreshDaemonLeavesForeignPtysAlone(t *testing.T) {
	jr, _ := newRecoveryRunner(t) // empty persister: this daemon owns no jobs

	sock := fakeTuimuxd(t, []tuimuxpty.SessionMetadata{
		agentPtyMeta("pty-1", "someone-elses-job-1"),
		agentPtyMeta("pty-2", "someone-elses-job-2"),
		agentPtyMeta("pty-3", ""), // no job identity at all
	})
	jr.tuimuxClient = &tuimux.ApiClient{SocketPath: sock}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jr.AdoptRunningAgents(ctx)

	if sessions := jr.store.GetSessions(); len(sessions) != 0 {
		t.Fatalf("a daemon with no persisted jobs must not adopt foreign PTYs, got %d sessions", len(sessions))
	}
}

// The legit case the filter must not break: groved upgrade/restart with the
// same state dir. The persisted job's PTY survived in tuimux and must be
// rebuilt into the session store exactly as before, while a foreign PTY on the
// same tuimux is still skipped.
func TestAdoptRestartRebuildsOwnedPtySessions(t *testing.T) {
	jr, persister := newRecoveryRunner(t)
	planDir, jobFile := planWithJob(t, "owned-job")
	persister.Save(&models.JobInfo{
		ID:      "owned-job",
		Type:    "interactive_agent",
		PlanDir: planDir,
		JobFile: jobFile,
		Status:  "running",
		PID:     deadPID, // launcher exited; the live PTY is the evidence of life
	})

	sock := fakeTuimuxd(t, []tuimuxpty.SessionMetadata{
		agentPtyMeta("pty-owned", "owned-job"),
		agentPtyMeta("pty-foreign", "someone-elses-job"),
	})
	jr.tuimuxClient = &tuimux.ApiClient{SocketPath: sock}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jr.AdoptRunningAgents(ctx)

	sess := jr.store.GetSession("owned-job")
	if sess == nil {
		t.Fatal("upgrade/restart must rebuild the session for a persisted job's surviving PTY")
	}
	if sess.PtyID != "pty-owned" {
		t.Fatalf("rebuilt session must carry the live PtyID, got %q", sess.PtyID)
	}
	if got := len(jr.store.GetSessions()); got != 1 {
		t.Fatalf("only the owned PTY may be rebuilt, got %d sessions", got)
	}

	job := reload(t, persister, "owned-job")
	if job.Status != "running" {
		t.Fatalf("a job with a live PTY must stay running, got %q (%s)", job.Status, job.Error)
	}
}

func TestRebuildAgentSessionsSkipsUnknownJobIDs(t *testing.T) {
	jr := newTestRunner(store.New())

	jr.rebuildAgentSessions(context.Background(), []tuimuxpty.SessionMetadata{
		agentPtyMeta("pty-1", "foreign-job"),
		agentPtyMeta("pty-2", ""),
	}, map[string]*models.JobInfo{"our-job": {ID: "our-job"}})

	if sessions := jr.store.GetSessions(); len(sessions) != 0 {
		t.Fatalf("PTYs with unknown or missing job_id must be left alone, got %d sessions", len(sessions))
	}
}

func TestRebuildAgentSessionsRejectsStalePriorAttemptPTY(t *testing.T) {
	jr := newTestRunner(store.New())
	stale := agentPtyMeta("pty-old", "reused-job")
	stale.Tags["attempt_id"] = "attempt-old"
	current := agentPtyMeta("pty-current", "reused-job")
	current.Tags["attempt_id"] = "attempt-current"

	jr.rebuildAgentSessions(context.Background(), []tuimuxpty.SessionMetadata{stale, current},
		map[string]*models.JobInfo{"reused-job": {ID: "reused-job", AttemptID: "attempt-current"}})

	sess := jr.store.GetSession("reused-job")
	if sess == nil || sess.AttemptID != "attempt-current" || sess.PtyID != "pty-current" {
		t.Fatalf("adopted session = %+v, want exact current attempt PTY", sess)
	}
}

func TestRebuildAgentSessionsSynthesizesOwnedSession(t *testing.T) {
	jr := newTestRunner(store.New())

	jr.rebuildAgentSessions(context.Background(), []tuimuxpty.SessionMetadata{
		agentPtyMeta("pty-1", "our-job"),
	}, map[string]*models.JobInfo{"our-job": {ID: "our-job"}})

	sess := jr.store.GetSession("our-job")
	if sess == nil {
		t.Fatal("an owned job's PTY must be synthesized into the session store")
	}
	if sess.PtyID != "pty-1" || sess.Type != "interactive_agent" {
		t.Fatalf("synthesized session malformed: pty_id=%q type=%q", sess.PtyID, sess.Type)
	}
}

// A recovered session still needs the persisted job's exact attempt identity;
// store presence alone cannot authorize a stale PTY from a prior retry.
func TestRebuildAgentSessionsUpdatesStoreRecoveredSession(t *testing.T) {
	st := store.New()
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Source: "test",
		Payload: []*models.Session{
			{ID: "recovered-job", Type: "interactive_agent", Status: "running"},
		},
	})
	jr := newTestRunner(st)

	jr.rebuildAgentSessions(context.Background(), []tuimuxpty.SessionMetadata{
		agentPtyMeta("pty-new", "recovered-job"),
	}, map[string]*models.JobInfo{"recovered-job": {ID: "recovered-job"}})

	sess := jr.store.GetSession("recovered-job")
	if sess == nil || sess.PtyID != "pty-new" {
		t.Fatalf("a scope-recovered session must get its PtyID rebound, got %+v", sess)
	}
}
