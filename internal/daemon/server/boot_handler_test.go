package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// unixHTTPClient returns an http.Client that dials the given unix socket for
// every request, so tests can drive the daemon exactly as a RemoteClient does.
func unixHTTPClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// TestBootEndpointAdvancesInBindMode models the early-bind cold boot: the
// socket is bound and serving BEFORE boot finishes, and GET /api/system/boot
// reports the advancing phase the whole time — with NO engine wired, proving
// the endpoint answers from the earliest serving moment. This is the daemon
// half of the treemux splash's loading screen.
func TestBootEndpointAdvancesInBindMode(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "groved.sock")
	s := New(false)
	// Deliberately no SetEngine — the boot endpoint must not depend on it.

	// Seed an in-progress status, then bind + serve (Listen/Serve split is the
	// mechanism the --ready-at=bind path uses in groved.go).
	s.SetBootStatus(&coredaemon.BootStatus{Phase: "tuimux", PhaseIndex: 1, PhaseTotal: 3})
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	client := unixHTTPClient(sock)

	get := func() coredaemon.BootStatus {
		t.Helper()
		var status coredaemon.BootStatus
		// The socket may accept before Serve's first Accept lands; retry briefly.
		var lastErr error
		for i := 0; i < 50; i++ {
			resp, err := client.Get("http://unix/api/system/boot")
			if err != nil {
				lastErr = err
				time.Sleep(20 * time.Millisecond)
				continue
			}
			err = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode boot status: %v", err)
			}
			return status
		}
		t.Fatalf("GET /api/system/boot never succeeded: %v", lastErr)
		return status
	}

	if got := get(); got.Phase != "tuimux" || got.PhaseIndex != 1 || got.Done {
		t.Fatalf("phase 1: got %+v, want {tuimux,1,not done}", got)
	}

	s.SetBootStatus(&coredaemon.BootStatus{Phase: "watchers", PhaseIndex: 2, PhaseTotal: 3})
	if got := get(); got.Phase != "watchers" || got.PhaseIndex != 2 || got.Done {
		t.Fatalf("phase 2: got %+v, want {watchers,2,not done}", got)
	}

	s.SetBootStatus(&coredaemon.BootStatus{Done: true, PhaseIndex: 3, PhaseTotal: 3})
	if got := get(); !got.Done {
		t.Fatalf("final: got %+v, want Done", got)
	}
}

// TestBootEndpointDefaultsDone proves the default bind-last ordering: no
// SetBootStatus was ever called, so the endpoint reports Done — a client only
// reaches it once the daemon is already serving, which under that ordering
// means boot already finished.
func TestBootEndpointDefaultsDone(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "groved.sock")
	s := New(false)
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	client := unixHTTPClient(sock)
	var status coredaemon.BootStatus
	for i := 0; i < 50; i++ {
		resp, err := client.Get("http://unix/api/system/boot")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = json.NewDecoder(resp.Body).Decode(&status)
		resp.Body.Close()
		break
	}
	if !status.Done {
		t.Fatalf("default endpoint: got %+v, want Done", status)
	}
}

// TestConvertToAPIUpdateBootPhase checks the store→SSE wire mapping: a
// UpdateBootPhase carries the typed BootStatus into apiStateUpdate.BootPhase
// (not the generic Payload), keeping it in sync with StateUpdate.BootPhase.
func TestConvertToAPIUpdateBootPhase(t *testing.T) {
	bs := &coredaemon.BootStatus{Phase: "environment", PhaseIndex: 3, PhaseTotal: 7}
	got := convertToAPIUpdate(store.Update{Type: store.UpdateBootPhase, Source: "boot", Payload: bs})
	if got == nil {
		t.Fatal("convertToAPIUpdate returned nil for boot_phase")
	}
	if got.UpdateType != "boot_phase" {
		t.Fatalf("update_type = %q, want boot_phase", got.UpdateType)
	}
	if got.BootPhase == nil || got.BootPhase.Phase != "environment" || got.BootPhase.PhaseIndex != 3 {
		t.Fatalf("BootPhase = %+v, want environment/3", got.BootPhase)
	}
	if got.Payload != nil {
		t.Fatalf("Payload should be nil for boot_phase, got %v", got.Payload)
	}
}

// TestBootPhaseBroadcastReachesStream drives the full path a treemux splash
// relies on: BroadcastBootPhase on the store → convertToAPIUpdate → the
// /api/stream SSE frame, decoding back into a StateUpdate with a populated
// boot_phase.
func TestBootPhaseBroadcastReachesStream(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "groved.sock")
	st := store.New()
	eng := engine.New(st)
	s := New(false)
	s.SetEngine(eng)
	if err := s.Listen(sock); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/api/stream", nil)
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
		t.Fatalf("subscribe /api/stream: %v", err)
	}
	defer resp.Body.Close()

	// Broadcast a phase once the subscription is live. A tiny delay lets the
	// handler register its store subscriber before we fan out.
	go func() {
		time.Sleep(100 * time.Millisecond)
		st.BroadcastBootPhase(&coredaemon.BootStatus{Phase: "watchers", PhaseIndex: 6, PhaseTotal: 7})
	}()

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var update coredaemon.StateUpdate
		if err := json.Unmarshal([]byte(payload), &update); err != nil {
			continue
		}
		if update.UpdateType == "boot_phase" {
			if update.BootPhase == nil || update.BootPhase.Phase != "watchers" || update.BootPhase.PhaseIndex != 6 {
				t.Fatalf("boot_phase frame BootPhase = %+v, want watchers/6", update.BootPhase)
			}
			return // success
		}
	}
	t.Fatal("never received a boot_phase SSE frame")
}
