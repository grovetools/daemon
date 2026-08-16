package server

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func newTestSocket(t *testing.T, path string) (net.Listener, os.FileInfo) {
	t.Helper()
	listener, info, err := bindPublishedUnixSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_, _ = removeSocketIfOwned(path, info)
	})
	return listener, info
}

func assertSocketIdentity(t *testing.T, path string, want os.FileInfo) {
	t.Helper()
	got, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if !os.SameFile(got, want) {
		t.Fatal("public path does not have expected socket identity")
	}
}

func TestPrivateSocketPathDoesNotLengthenPublicPath(t *testing.T) {
	public := shortSocketPath(t)
	private, err := privateSocketPath(public)
	if err != nil {
		t.Fatal(err)
	}
	if len(private) > len(public) {
		t.Fatalf("private socket path grew from %d to %d bytes: %q", len(public), len(private), private)
	}
}

func TestSocketIdentityHealthy(t *testing.T) {
	path := shortSocketPath(t)
	s := New(false)
	if err := s.Listen(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	if lost, detail := s.SocketIdentityLost(); lost {
		t.Fatalf("healthy listener reported lost: %s", detail)
	}
}

func TestSocketIdentityDetectsPathReplacement(t *testing.T) {
	path := shortSocketPath(t)
	s := New(false)
	if err := s.Listen(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	_, successor := newTestSocket(t, path)

	if lost, _ := s.SocketIdentityLost(); !lost {
		t.Fatal("server failed to detect deterministic socket replacement")
	}
	assertSocketIdentity(t, path, successor)
}

func TestPublishedSocketAcceptsConnectionsAfterRename(t *testing.T) {
	path := shortSocketPath(t)
	listener, _ := newTestSocket(t, path)

	accepted := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_, err = conn.Write([]byte("ok"))
		}
		accepted <- err
	}()

	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial renamed public path: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 2)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read from renamed socket: %v", err)
	}
	if string(buf) != "ok" {
		t.Fatalf("received %q, want ok", buf)
	}
	if err := <-accepted; err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestShutdownRemovesOnlyOwnedSocket(t *testing.T) {
	t.Run("owned", func(t *testing.T) {
		path := shortSocketPath(t)
		s := New(false)
		if err := s.Listen(path); err != nil {
			t.Fatal(err)
		}

		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("owned socket survived shutdown: %v", err)
		}
	})

	t.Run("successor", func(t *testing.T) {
		path := shortSocketPath(t)
		s := New(false)
		if err := s.Listen(path); err != nil {
			t.Fatal(err)
		}
		_, successor := newTestSocket(t, path)

		if err := s.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertSocketIdentity(t, path, successor)
	})
}

func TestCleanupInterleavingPreservesSuccessorSocket(t *testing.T) {
	path := shortSocketPath(t)
	original, originalIdentity := newTestSocket(t, path)
	defer original.Close()

	var successor net.Listener
	var successorIdentity os.FileInfo
	removed, err := removeSocketIfOwnedBeforeDetach(path, originalIdentity, func() {
		var bindErr error
		successor, successorIdentity, bindErr = bindPublishedUnixSocket(path)
		if bindErr != nil {
			t.Fatalf("publish successor: %v", bindErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("cleanup reported removing its own socket after successor publication")
	}
	t.Cleanup(func() {
		_ = successor.Close()
		_, _ = removeSocketIfOwned(path, successorIdentity)
	})
	assertSocketIdentity(t, path, successorIdentity)

	accepted := make(chan error, 1)
	go func() {
		conn, err := successor.Accept()
		if err == nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial restored successor: %v", err)
	}
	_ = conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("accept restored successor: %v", err)
	}
}

func TestDrainDoesNotRemoveSuccessorSocket(t *testing.T) {
	path := shortSocketPath(t)
	s := New(false)
	if err := s.Listen(path); err != nil {
		t.Fatal(err)
	}
	_, successor := newTestSocket(t, path)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // EnterDrainMode still performs cleanup, but skips its 30s wait.
	s.EnterDrainMode(ctx)
	assertSocketIdentity(t, path, successor)
}
