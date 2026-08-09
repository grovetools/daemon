package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/daemon/internal/daemon/engine"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// TestPprofServedOnUnixSocket pins the observability contract this endpoint
// exists for: a heap profile can be pulled from an ALREADY-RUNNING daemon over
// its socket. Before this, profiling required restarting groved with
// --pprof-port, which kills every live agent pane — so the daemons that
// actually misbehaved were never the ones profiled, and heap retention was
// guessed at from goroutine counts for weeks.
//
// The assertion is deliberately "the bytes parse as a pprof heap profile with
// the four standard sample types", not "status 200": a mux that 404s returns a
// perfectly valid 404 body, and an Index handler mounted without its subpaths
// returns the HTML profile list for /heap. Both would pass a status check.
func TestPprofServedOnUnixSocket(t *testing.T) {
	sock := shortSocketPath(t)
	s := New(false)
	s.SetEngine(engine.New(store.New()))
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
	resp, err := client.Get("http://unix/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/pprof/heap = %d, want 200", resp.StatusCode)
	}

	// The wire format is a gzipped profile.proto. Decoding the proto would mean
	// taking on github.com/google/pprof as a dependency for one assertion, so
	// instead: it must gunzip (a 404 or the Index HTML will not), and the
	// decompressed bytes must carry the heap sample-type names, which live
	// verbatim in the proto's string table.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("response body is not gzipped pprof output: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	// inuse_space is the sample type this job's attribution reads.
	for _, want := range []string{"inuse_space", "alloc_space", "space", "bytes"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("heap profile string table has no %q — this does not look like a heap profile", want)
		}
	}

	// The named-profile subpaths route through Index; goroutine?debug=1 is the
	// other one every audit reaches for, and it renders as text rather than a
	// gzipped proto, so it also proves query params survive the mount.
	gr, err := client.Get("http://unix/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("GET /debug/pprof/goroutine: %v", err)
	}
	defer func() { _ = gr.Body.Close() }()
	body, _ := io.ReadAll(gr.Body)
	if !strings.Contains(string(body), "goroutine profile:") {
		t.Errorf("goroutine?debug=1 body did not look like a goroutine dump; got %.120q", body)
	}
}

// TestPprofRejectedOnTCPListener holds the security half of the contract. The
// same mux is served on an unauthenticated localhost TCP listener for the web
// terminal viewer; profiles expose full symbolized stacks and (via /cmdline)
// the daemon's argv, so they must stay socket-only. Mounting them bare on the
// mux — the obvious way to do it — would have published them on that port.
func TestPprofRejectedOnTCPListener(t *testing.T) {
	port := freeTCPPort(t)
	sock := shortSocketPath(t)
	s := New(false)
	s.SetEngine(engine.New(store.New()))
	if err := s.Listen(sock, port); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = s.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	url := "http://localhost:" + strconv.Itoa(port)

	// The TCP listener starts in a goroutine; retry briefly rather than racing.
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = client.Get(url + "/health")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if err != nil {
		t.Skipf("TCP listener never came up (%v) — cannot assert the negative", err)
	}

	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
	} {
		r, err := client.Get(url + path)
		if err != nil {
			t.Errorf("GET %s over TCP: %v", path, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 512))
		_ = r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s over TCP = %d, want 403 (pprof must not be reachable off the unix socket); body %.120q",
				path, r.StatusCode, body)
		}
	}
}

// freeTCPPort returns a localhost port that was free a moment ago. Racy in
// principle; the alternative is not testing the TCP negative at all.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
