package env

import (
	"sync"
	"testing"
)

func TestPortAllocator_Allocate(t *testing.T) {
	pa := NewPortAllocator()

	port, err := pa.Allocate("test/web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port <= 0 {
		t.Fatalf("expected positive port, got %d", port)
	}
}

func TestPortAllocator_UniqueAllocations(t *testing.T) {
	pa := NewPortAllocator()

	ports := make(map[int]bool)
	for i := 0; i < 10; i++ {
		port, err := pa.Allocate("test/svc")
		if err != nil {
			t.Fatalf("allocation %d failed: %v", i, err)
		}
		if ports[port] {
			t.Fatalf("duplicate port allocated: %d", port)
		}
		ports[port] = true
	}
}

func TestPortAllocator_Release(t *testing.T) {
	pa := NewPortAllocator()

	port, err := pa.Allocate("test/web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pa.Release(port)

	// After release, the used map should not contain the port
	pa.mu.Lock()
	_, exists := pa.used[port]
	pa.mu.Unlock()
	if exists {
		t.Errorf("port %d should have been released", port)
	}
}

func TestPortAllocator_ReleaseAll(t *testing.T) {
	pa := NewPortAllocator()

	// Allocate ports for two different worktrees
	for i := 0; i < 3; i++ {
		if _, err := pa.Allocate("wt-a/svc"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := pa.Allocate("wt-b/svc"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	pa.ReleaseAll("wt-a")

	pa.mu.Lock()
	remaining := len(pa.used)
	pa.mu.Unlock()

	if remaining != 2 {
		t.Errorf("expected 2 remaining ports after ReleaseAll(wt-a), got %d", remaining)
	}
}

func TestPortAllocator_ConcurrentAllocations(t *testing.T) {
	pa := NewPortAllocator()

	var wg sync.WaitGroup
	results := make(chan int, 20)
	errs := make(chan error, 20)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			port, err := pa.Allocate("concurrent/svc")
			if err != nil {
				errs <- err
				return
			}
			results <- port
		}(i)
	}

	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent allocation error: %v", err)
	}

	ports := make(map[int]bool)
	for port := range results {
		if ports[port] {
			t.Errorf("duplicate port from concurrent allocation: %d", port)
		}
		ports[port] = true
	}
}
