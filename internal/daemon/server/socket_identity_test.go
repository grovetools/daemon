package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestBoundListenerIdentitySurvivesPathReplacement deterministically exercises
// the bind-to-capture race: the pathname is stolen before identity capture, but
// the listener FD must still identify the original socket.
func TestBoundListenerIdentitySurvivesPathReplacement(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "groved-sockid-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "groved.sock")
	original, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.Close() }()

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	thief, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = thief.Close() }()

	bound, err := boundListenerInfo(original)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(bound, current) {
		t.Fatal("listener identity incorrectly followed the replaced pathname")
	}

	s := &Server{socketPath: path, boundSocket: bound}
	if lost, _ := s.SocketIdentityLost(); !lost {
		t.Fatal("server failed to detect socket replaced before identity capture")
	}
}
