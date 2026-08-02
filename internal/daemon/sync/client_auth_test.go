package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// TestAuthErrorClassification is the discriminator the stale-token trap needed
// (contract §3 P2b): a rejected TOKEN and an unreachable SERVER arrive at the
// transport loop as errors, and only one of them can ever be fixed by
// retrying. Before this, both were plain fmt errors and transportLoop logged
// both at debug — a recreated server produced an infinite silent 401 loop.
func TestAuthErrorClassification(t *testing.T) {
	ctx := context.Background()
	log := logging.NewUnifiedLogger("test.clientauth")

	statusServer := func(code int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", code)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("401 handshakes are auth errors", func(t *testing.T) {
		srv := statusServer(http.StatusUnauthorized)
		_, err := NewClientFromConfig(ctx, &config.SyncConfig{Server: srv.URL, Token: "stale"},
			"dev", "origin", "", log)
		if err == nil {
			t.Fatal("expected a handshake failure")
		}
		if !IsAuthError(err) {
			t.Fatalf("IsAuthError = false for %v", err)
		}
	})

	t.Run("other failures are not auth errors", func(t *testing.T) {
		// 403 above all: it is the server's authorization answer for a token it
		// RECOGNIZES, on a workspace the user has no grant for. Reading it as a
		// dead token tells operators of valid share-scoped clients to mint a
		// replacement and puts their transport in a reconnect cycle.
		for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError} {
			srv := statusServer(code)
			_, err := NewClientFromConfig(ctx, &config.SyncConfig{Server: srv.URL, Token: "t"},
				"dev", "origin", "", log)
			if err == nil || IsAuthError(err) {
				t.Fatalf("a %d must not read as a token rejection: %v", code, err)
			}
		}

		// An unreachable server: the case that legitimately fixes itself by
		// retrying, and must never be reported as a dead token.
		down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := down.URL
		down.Close()
		_, err := NewClientFromConfig(ctx, &config.SyncConfig{Server: url, Token: "t"}, "dev", "origin", "", log)
		if err == nil || IsAuthError(err) {
			t.Fatalf("an unreachable server must not read as a token rejection: %v", err)
		}
	})
}

// TestAuthFailureHookFiresForLivePipelines covers the half the handshake
// cannot see: transportLoop caches a connected client and never re-handshakes,
// so a token revoked (or a server recreated) UNDER a running daemon only ever
// surfaces on a live push/pull/snapshot. The hook is how those reach the
// transport owner; without it the pipelines 401-ed forever and the only cure
// was a daemon restart.
func TestAuthFailureHookFiresForLivePipelines(t *testing.T) {
	ctx := context.Background()

	// Handshake succeeds; everything afterwards is rejected — precisely the
	// mid-run revocation shape.
	var handshakes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sync/capabilities" {
			handshakes++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"capabilities":{"protocol_versions":[` + itoa(syncproto.ProtocolVersion) + `]}}`))
			return
		}
		http.Error(w, "token revoked", http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "was-good", OriginID: "origin"})
	if _, err := client.Capabilities(ctx, ""); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	var hookErrs []error
	client.SetAuthFailureHook(func(err error) { hookErrs = append(hookErrs, err) })

	if _, err := client.Push(ctx, "default", nil); !IsAuthError(err) {
		t.Fatalf("push: IsAuthError = false for %v", err)
	}
	if _, err := client.Snapshot(ctx, "default"); !IsAuthError(err) {
		t.Fatalf("snapshot: IsAuthError = false for %v", err)
	}
	if _, err := client.PullEvents(ctx, "default", 0, 10, 0); !IsAuthError(err) {
		t.Fatalf("pull: IsAuthError = false for %v", err)
	}
	if err := client.PushBlob(ctx, "deadbeef", []byte("x")); !IsAuthError(err) {
		t.Fatalf("blob upload: IsAuthError = false for %v", err)
	}

	if len(hookErrs) != 4 {
		t.Fatalf("expected the hook to fire for every rejected call, got %d", len(hookErrs))
	}
	for _, err := range hookErrs {
		if !IsAuthError(err) {
			t.Fatalf("hook received an unclassified error: %v", err)
		}
	}

	// The hook is optional and clearable — a cleared hook must not panic the
	// request path.
	client.SetAuthFailureHook(nil)
	if _, err := client.Snapshot(ctx, "default"); !IsAuthError(err) {
		t.Fatalf("snapshot after clearing the hook: %v", err)
	}
	if handshakes != 1 {
		t.Fatalf("the client must not silently re-handshake: %d handshakes", handshakes)
	}
}

// itoa avoids pulling strconv in for one call site in a literal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
