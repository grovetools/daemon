package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/satellite"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// fakeRemote is a stand-in satellite daemon served over a unix socket: it
// answers GET /api/jobs and /api/sessions from mutable canned state and pushes a
// "sessions" SSE frame on /api/stream whenever poke() is called.
type fakeRemote struct {
	mu       sync.Mutex
	jobs     []*models.JobInfo
	sessions []*models.Session
	poke     chan struct{}
	srv      *http.Server
	ln       net.Listener
}

func newFakeRemote(t *testing.T, sock string) *fakeRemote {
	t.Helper()
	f := &fakeRemote{poke: make(chan struct{}, 8)}
	f.serve(t, sock)
	return f
}

func (f *fakeRemote) setState(jobs []*models.JobInfo, sessions []*models.Session) {
	f.mu.Lock()
	f.jobs = jobs
	f.sessions = sessions
	f.mu.Unlock()
}

func (f *fakeRemote) serve(t *testing.T, sock string) {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		jobs := f.jobs
		f.mu.Unlock()
		if jobs == nil {
			jobs = []*models.JobInfo{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobs)
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		sessions := f.sessions
		f.mu.Unlock()
		if sessions == nil {
			sessions = []*models.Session{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sessions)
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case <-f.poke:
				fmt.Fprint(w, "data: {\"update_type\":\"sessions\"}\n\n")
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
		}
	})
	f.srv = &http.Server{Handler: mux}
	f.ln = ln
	go f.srv.Serve(ln)
}

func (f *fakeRemote) stop() {
	if f.srv != nil {
		f.srv.Close()
	}
	if f.ln != nil {
		f.ln.Close()
	}
}

// fakeDialer dials the fake remote's unix socket, satisfying SocketDialer.
type fakeDialer struct{ sock string }

func (d fakeDialer) DialSatelliteSocket(name string) (net.Conn, error) {
	return net.Dial("unix", d.sock)
}

func shortTempSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "satcoll")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// newTestCollector builds a collector with fast timings and a one-entry registry.
func newTestCollector(dialer SocketDialer) *SatelliteCollector {
	reg := satellite.NewRegistry(map[string]*satellite.SatelliteConfig{
		"sat": {SSHAddr: "ignored", User: "u"},
	})
	c := NewSatelliteCollector(dialer, reg)
	c.retryInterval = 40 * time.Millisecond
	c.snapshotDebounce = 30 * time.Millisecond
	return c
}

func seedConnected(st *store.Store, name string) {
	st.ApplyUpdate(store.Update{
		Type:    store.UpdateSatelliteStatus,
		Payload: &store.SatelliteStatusPayload{Name: name, State: "connected"},
	})
}

// waitFor polls cond up to d, returning true if it became true.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func satJobIDs(st *store.Store, origin string) map[string]bool {
	out := map[string]bool{}
	for _, j := range st.GetJobs() {
		if j.Origin == origin {
			out[j.ID] = true
		}
	}
	return out
}

// TestSatelliteCollectorFederatesAndReconciles is the end-to-end path: a remote
// job lands with Origin set and its ANSI title sanitized; an SSE event drives a
// re-snapshot that picks up a change; killing the server leaves rows present
// (stale-not-deleted); and a reconnect reconciles a removed job away.
func TestSatelliteCollectorFederatesAndReconciles(t *testing.T) {
	sock := shortTempSock(t)
	f := newFakeRemote(t, sock)
	defer f.stop()
	f.setState(
		[]*models.JobInfo{{ID: "A", Status: "running", Title: "\x1b[31mred\x1b[0m job"}},
		[]*models.Session{{ID: "A", Status: "running"}},
	)

	st := store.New()
	seedConnected(st, "sat")

	c := newTestCollector(fakeDialer{sock: sock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 32)
	// Wire the collector's updates into the store, mirroring engine.Start.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-updates:
				st.ApplyUpdate(u)
			}
		}
	}()
	go c.Run(ctx, st, updates)

	// 1. Remote job A lands with Origin and a sanitized title.
	if !waitFor(3*time.Second, func() bool { return satJobIDs(st, "sat")["A"] }) {
		t.Fatal("remote job A never federated")
	}
	var jobA *models.JobInfo
	for _, j := range st.GetJobs() {
		if j.Origin == "sat" && j.ID == "A" {
			jobA = j
		}
	}
	if jobA == nil {
		t.Fatal("job A missing")
	}
	if jobA.Title != "red job" {
		t.Fatalf("title not sanitized: %q", jobA.Title)
	}

	// 2. SSE event → debounced re-snapshot picks up a newly added job B.
	f.setState(
		[]*models.JobInfo{
			{ID: "A", Status: "running", Title: "a"},
			{ID: "B", Status: "running", Title: "b"},
		},
		[]*models.Session{{ID: "A", Status: "running"}},
	)
	f.poke <- struct{}{}
	if !waitFor(3*time.Second, func() bool { return satJobIDs(st, "sat")["B"] }) {
		t.Fatal("SSE event did not trigger a re-snapshot that picked up job B")
	}

	// 3. Kill the server → rows persist (stale, not deleted).
	f.stop()
	time.Sleep(200 * time.Millisecond)
	if !satJobIDs(st, "sat")["A"] || !satJobIDs(st, "sat")["B"] {
		t.Fatal("federated rows were deleted on disconnect (must be stale-not-deleted)")
	}

	// 4. Restart with B removed → reconnect reconciles B away, A stays.
	f2 := newFakeRemote(t, sock)
	defer f2.stop()
	f2.setState(
		[]*models.JobInfo{{ID: "A", Status: "running", Title: "a"}},
		[]*models.Session{{ID: "A", Status: "running"}},
	)
	if !waitFor(3*time.Second, func() bool {
		ids := satJobIDs(st, "sat")
		return ids["A"] && !ids["B"]
	}) {
		t.Fatalf("reconnect did not reconcile: got %v", satJobIDs(st, "sat"))
	}
}

// TestSatelliteCollectorSkipsWhenDisconnected proves the gate: with no connected
// satellite_status, the collector emits nothing.
func TestSatelliteCollectorSkipsWhenDisconnected(t *testing.T) {
	sock := shortTempSock(t)
	f := newFakeRemote(t, sock)
	defer f.stop()
	f.setState([]*models.JobInfo{{ID: "A", Status: "running"}}, nil)

	st := store.New() // no satellite_status seeded → not connected
	c := newTestCollector(fakeDialer{sock: sock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case u := <-updates:
				st.ApplyUpdate(u)
			}
		}
	}()
	go c.Run(ctx, st, updates)

	time.Sleep(300 * time.Millisecond)
	if len(satJobIDs(st, "sat")) != 0 {
		t.Fatal("collector federated rows despite the satellite not being connected")
	}
}

// TestReaperLeavesRemoteSessionsAlone is the C8 reaper guard: a federated session
// with a dead PID and old activity must never be reaped (no UpdateSessionEnd).
func TestReaperLeavesRemoteSessionsAlone(t *testing.T) {
	st := store.New()
	// A remote session that would look reapable to the liveness loop: dead PID,
	// old activity, "running" status.
	st.ApplyUpdate(store.Update{
		Type:   store.UpdateSessions,
		Origin: "sat",
		Payload: []*models.Session{{
			ID:           "R",
			Origin:       "sat",
			Status:       "running",
			PID:          999999, // not a live process
			StartedAt:    time.Now().Add(-10 * time.Minute),
			LastActivity: time.Now().Add(-10 * time.Minute),
		}},
	})

	sc := NewSessionCollector(20*time.Millisecond, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan store.Update, 16)
	go sc.Run(ctx, st, updates)

	deadline := time.After(400 * time.Millisecond)
	for {
		select {
		case u := <-updates:
			if u.Type == store.UpdateSessionEnd {
				if p, ok := u.Payload.(*store.SessionEndPayload); ok && p.JobID == "R" {
					t.Fatal("reaper emitted UpdateSessionEnd for a remote session (C8 violated)")
				}
			}
		case <-deadline:
			return // no reap of the remote session — success
		}
	}
}
