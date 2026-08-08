package sync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/grovetools/core/pkg/syncproto"
)

type testDeviceSigner struct {
	id      string
	private ed25519.PrivateKey
}

func newTestDeviceSigner(t *testing.T, id string) (*testDeviceSigner, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testDeviceSigner{id: id, private: private}, public
}

func (s *testDeviceSigner) DeviceID() string     { return s.id }
func (s *testDeviceSigner) Sign(p []byte) []byte { return ed25519.Sign(s.private, p) }

func writeDeviceCapabilities(t *testing.T, w http.ResponseWriter, r *http.Request, public ed25519.PublicKey, session string) {
	t.Helper()
	var req syncproto.CapabilitiesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode capabilities: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := syncproto.VerifyCapabilities(req, public); err != nil {
		t.Errorf("verify capabilities: %v", err)
		http.Error(w, "bad proof", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
		ServerEpoch:     "epoch-1",
		ProtocolVersion: syncproto.ProtocolVersionDeviceSession,
		Capabilities: syncproto.Capabilities{
			ProtocolVersions: syncproto.SupportedProtocolVersions(),
		},
		SessionToken: session,
	})
}

func TestDeviceSessionRefreshCoversEveryBearerUse(t *testing.T) {
	ctx := context.Background()
	signer, public := newTestDeviceSigner(t, "device-1")
	var handshakes atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/identity":
			_ = json.NewEncoder(w).Encode(syncproto.IdentityResponse{
				ServerEpoch: "epoch-1", ProtocolVersions: syncproto.SupportedProtocolVersions(),
			})
		case "/sync/capabilities":
			n := handshakes.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("device handshake carried Authorization header")
			}
			writeDeviceCapabilities(t, w, r, public, "session-"+itoa(int(n)))
		default:
			auth := r.Header.Get("Authorization")
			if auth == "Bearer session-1" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			if auth != "Bearer session-2" {
				t.Errorf("%s %s used %q", r.Method, r.URL.Path, auth)
				http.Error(w, "wrong bearer", http.StatusUnauthorized)
				return
			}
			switch {
			case r.URL.Path == "/sync/push":
				_, _ = w.Write([]byte(`{}`))
			case r.URL.Path == "/sync/snapshot":
				_, _ = w.Write([]byte(`{"documents":[]}`))
			case r.URL.Path == "/sync/events":
				_, _ = w.Write([]byte(`{"events":[]}`))
			case r.URL.Path == "/sync/history":
				_, _ = w.Write([]byte(`[]`))
			case r.URL.Path == "/sync/history/blob":
				_, _ = w.Write([]byte("old"))
			case r.URL.Path == "/sync/blob/hash" && r.Method == http.MethodPost:
				w.WriteHeader(http.StatusCreated)
			default:
				if r.Method == http.MethodGet && len(r.URL.Path) > len("/sync/blob/") && r.URL.Path[:len("/sync/blob/")] == "/sync/blob/" {
					_, _ = w.Write([]byte("x"))
					return
				}
				http.NotFound(w, r)
			}
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, DeviceID: signer.id, OriginID: "origin", Signer: signer})
	if _, err := client.Capabilities(ctx, "test"); err != nil {
		t.Fatalf("initial capabilities: %v", err)
	}
	if _, err := client.Push(ctx, "default", nil); err != nil {
		t.Fatalf("push refresh: %v", err)
	}
	if _, err := client.Snapshot(ctx, "default"); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := client.PullEvents(ctx, "default", 0, 10, 0); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if err := client.PushBlob(ctx, "hash", []byte("x")); err != nil {
		t.Fatalf("push blob: %v", err)
	}
	hash := sha256.Sum256([]byte("x"))
	if _, err := client.FetchBlob(ctx, hex.EncodeToString(hash[:])); err != nil {
		t.Fatalf("fetch blob: %v", err)
	}
	if _, err := client.History(ctx, "default", "note.md"); err != nil {
		t.Fatalf("history: %v", err)
	}
	if _, err := client.HistoryBlob(ctx, "default", "doc", 1); err != nil {
		t.Fatalf("history blob: %v", err)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes = %d, want initial + one refresh", got)
	}
}

func TestDeviceSessionConcurrent401sCoalesceRefresh(t *testing.T) {
	ctx := context.Background()
	signer, public := newTestDeviceSigner(t, "device-1")
	const workers = 8
	var handshakes atomic.Int32
	var rejected atomic.Int32
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/identity":
			_ = json.NewEncoder(w).Encode(syncproto.IdentityResponse{ServerEpoch: "epoch-1", ProtocolVersions: syncproto.SupportedProtocolVersions()})
		case "/sync/capabilities":
			n := handshakes.Add(1)
			writeDeviceCapabilities(t, w, r, public, "session-"+itoa(int(n)))
		case "/sync/snapshot":
			if r.Header.Get("Authorization") == "Bearer session-1" {
				if rejected.Add(1) == workers {
					close(release)
				}
				<-release
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer session-2" {
				http.Error(w, "wrong bearer", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"documents":[]}`))
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, DeviceID: signer.id, Signer: signer})
	if _, err := client.Capabilities(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Snapshot(ctx, "default")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("snapshot: %v", err)
		}
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes = %d, want initial + one coalesced refresh", got)
	}
}

func TestDeviceSessionDoesNotFallBackWithoutLegacyToken(t *testing.T) {
	signer, _ := newTestDeviceSigner(t, "device-1")
	var capabilityCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync/identity" {
			http.Error(w, "old server", http.StatusNotFound)
			return
		}
		capabilityCalls.Add(1)
		http.Error(w, "unexpected fallback", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, DeviceID: signer.id, Signer: signer})
	if _, err := client.Capabilities(context.Background(), "test"); err == nil {
		t.Fatal("expected identity discovery failure")
	}
	if got := capabilityCalls.Load(); got != 0 {
		t.Fatalf("v1 fallback calls = %d, want 0 without legacy token", got)
	}
}

func TestDeviceSessionTransientRefreshFailureRecovers(t *testing.T) {
	ctx := context.Background()
	signer, public := newTestDeviceSigner(t, "device-1")
	var identities atomic.Int32
	var handshakes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/identity":
			if identities.Add(1) == 2 {
				http.Error(w, "identity temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(syncproto.IdentityResponse{ServerEpoch: "epoch-1", ProtocolVersions: syncproto.SupportedProtocolVersions()})
		case "/sync/capabilities":
			n := handshakes.Add(1)
			writeDeviceCapabilities(t, w, r, public, "session-"+itoa(int(n)))
		case "/sync/snapshot":
			if r.Header.Get("Authorization") == "Bearer session-1" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"documents":[]}`))
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, DeviceID: signer.id, Signer: signer})
	if _, err := client.Capabilities(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Snapshot(ctx, "default"); err == nil {
		t.Fatal("first snapshot unexpectedly survived transient refresh failure")
	}
	if _, err := client.Snapshot(ctx, "default"); err != nil {
		t.Fatalf("later snapshot did not retry and recover: %v", err)
	}
	if got := identities.Load(); got != 3 {
		t.Fatalf("identity calls = %d, want initial + failed refresh + recovery", got)
	}
	if got := handshakes.Load(); got != 2 {
		t.Fatalf("signed handshakes = %d, want initial + recovered refresh", got)
	}
}

func TestDeviceSessionFallsBackToV1OnlyWithLegacyToken(t *testing.T) {
	signer, _ := newTestDeviceSigner(t, "device-1")
	var capabilityCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/identity":
			http.Error(w, "old server", http.StatusNotFound)
		case "/sync/capabilities":
			capabilityCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer legacy" {
				http.Error(w, "missing legacy bearer", http.StatusUnauthorized)
				return
			}
			var req syncproto.CapabilitiesRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.ProtocolVersions) != 1 || req.ProtocolVersions[0] != syncproto.ProtocolVersionLegacy || req.Signature != "" {
				http.Error(w, "not a v1 request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(syncproto.CapabilitiesResponse{
				ProtocolVersion: syncproto.ProtocolVersionLegacy,
				Capabilities:    syncproto.Capabilities{ProtocolVersions: []int{syncproto.ProtocolVersionLegacy}},
			})
		}
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, DeviceID: signer.id, Signer: signer, Token: "legacy"})
	if _, err := client.Capabilities(context.Background(), "test"); err != nil {
		t.Fatalf("v1 fallback: %v", err)
	}
	if got := capabilityCalls.Load(); got != 1 {
		t.Fatalf("v1 capability calls = %d, want 1", got)
	}
}
