package env

import (
	"fmt"
	"net"
	"strings"
	"sync"
)

// PortAllocator provides concurrency-safe ephemeral port allocation.
type PortAllocator struct {
	mu   sync.Mutex
	used map[int]string // Maps port to a label (e.g., "worktree/service")
}

func NewPortAllocator() *PortAllocator {
	return &PortAllocator{
		used: make(map[int]string),
	}
}

// Allocate requests an ephemeral port from the OS and marks it as used.
func (pa *PortAllocator) Allocate(label string) (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Try up to 5 times to prevent race conditions with external processes
	for i := 0; i < 5; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()

		if _, exists := pa.used[port]; !exists {
			pa.used[port] = label
			return port, nil
		}
	}
	return 0, fmt.Errorf("failed to allocate ephemeral port for %s", label)
}

// Release frees a specific port.
func (pa *PortAllocator) Release(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.used, port)
}

// Reserve marks a known port as used without allocating a new one.
// Used during daemon restore to prevent collisions with previously allocated ports.
func (pa *PortAllocator) Reserve(label string, port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pa.used[port] = label
}

// ReleaseAll frees all ports associated with a given prefix (e.g., a worktree name).
func (pa *PortAllocator) ReleaseAll(prefix string) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	for port, label := range pa.used {
		if strings.HasPrefix(label, prefix+"/") {
			delete(pa.used, port)
		}
	}
}
