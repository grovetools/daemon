package env

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/daemon"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

// fakeProxyClient records RegisterProxyRoute / UnregisterProxyRoutes calls
// and otherwise delegates every other Client method to LocalClient (which
// returns "requires daemon" errors). The scoped-daemon delegation paths
// under test only ever exercise the proxy methods, so that's enough.
type fakeProxyClient struct {
	*daemon.LocalClient
	registered   []proxyCall
	unregistered []string
	registerErr  error
}

type proxyCall struct {
	Worktree string
	Route    string
	Port     int
}

func newFakeProxyClient() *fakeProxyClient {
	return &fakeProxyClient{LocalClient: daemon.NewLocalClient()}
}

func (f *fakeProxyClient) RegisterProxyRoute(ctx context.Context, worktree, route string, port int) error {
	f.registered = append(f.registered, proxyCall{Worktree: worktree, Route: route, Port: port})
	return f.registerErr
}

func (f *fakeProxyClient) UnregisterProxyRoutes(ctx context.Context, worktree string) error {
	f.unregistered = append(f.unregistered, worktree)
	return nil
}

// TestManager_ScopedDelegatesProxyRegisterToGlobalClient verifies that when
// a scoped daemon has a globalClient set, registerProxyRoute RPCs instead
// of writing to the local m.Proxy map.
func TestManager_ScopedDelegatesProxyRegisterToGlobalClient(t *testing.T) {
	m := NewManager()
	fake := newFakeProxyClient()
	m.SetGlobalClient(fake)

	m.registerProxyRoute(context.Background(), "tier1-c", "api", 40123)

	if len(fake.registered) != 1 {
		t.Fatalf("want 1 register call, got %d", len(fake.registered))
	}
	got := fake.registered[0]
	if got.Worktree != "tier1-c" || got.Route != "api" || got.Port != 40123 {
		t.Errorf("register payload = %+v, want {tier1-c api 40123}", got)
	}
	// The local m.Proxy table should be untouched on scoped daemons — the
	// global daemon is the single authoritative proxy.
	if _, ok := m.Proxy.Lookup("api.tier1-c.grove.local"); ok {
		t.Error("scoped daemon should not write to local m.Proxy, but lookup succeeded")
	}
}

// TestManager_GlobalDaemonWritesLocalProxy verifies that when no globalClient
// is wired up (manager is running as the global daemon), registerProxyRoute
// hits the local m.Proxy map.
func TestManager_GlobalDaemonWritesLocalProxy(t *testing.T) {
	m := NewManager()
	// No SetGlobalClient — simulates the global daemon.

	m.registerProxyRoute(context.Background(), "tier1-c", "api", 41001)

	target, ok := m.Proxy.Lookup("api.tier1-c.grove.local")
	if !ok {
		t.Fatalf("expected local m.Proxy to hold the route")
	}
	if target != "127.0.0.1:41001" {
		t.Errorf("target = %q, want 127.0.0.1:41001", target)
	}
}

// TestManager_ScopedDelegatesUnregisterToGlobalClient mirrors the above for
// unregister.
func TestManager_ScopedDelegatesUnregisterToGlobalClient(t *testing.T) {
	m := NewManager()
	fake := newFakeProxyClient()
	m.SetGlobalClient(fake)

	m.unregisterProxyRoutes(context.Background(), "tier1-c")

	if len(fake.unregistered) != 1 || fake.unregistered[0] != "tier1-c" {
		t.Errorf("unregister calls = %v, want [tier1-c]", fake.unregistered)
	}
}

// TestManager_RPCFailureIsNonFatal verifies that a failing globalClient.RegisterProxyRoute
// does not crash — the env is still usable by direct port, only the
// *.grove.local hostname routing degrades. The spec explicitly requires
// Up not to fail on RPC errors.
func TestManager_RPCFailureIsNonFatal(t *testing.T) {
	m := NewManager()
	fake := newFakeProxyClient()
	fake.registerErr = fmt.Errorf("global daemon unreachable")
	m.SetGlobalClient(fake)

	// Must not panic or return anything — helper is void.
	m.registerProxyRoute(context.Background(), "tier1-c", "api", 42000)

	if len(fake.registered) != 1 {
		t.Errorf("register call still recorded despite error: got %d calls", len(fake.registered))
	}
}

// TestRestore_RegistersProxyRoutesFromStateFile verifies the global-daemon-boot
// restore path: every state.json with ProxyRoutes populated rebuilds m.Proxy
// so the :8443 listener can route to the same targets across a daemon restart.
func TestRestore_RegistersProxyRoutesFromStateFile(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}

	stateFile := coreenv.EnvStateFile{
		Provider:      "docker",
		WorkspaceName: "tier1-c",
		WorkspacePath: wtPath,
		Ports:         map[string]int{"api": 45000, "web": 45001},
		ProxyRoutes:   map[string]int{"api": 45000, "web": 45001},
	}
	data, _ := json.MarshalIndent(&stateFile, "", "  ")
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), data, 0644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}

	m := NewManager()
	m.Restore([]string{tmp})

	apiTarget, ok := m.Proxy.Lookup("api.tier1-c.grove.local")
	if !ok {
		t.Fatalf("api route not restored from state.json")
	}
	if apiTarget != "127.0.0.1:45000" {
		t.Errorf("api target = %q, want 127.0.0.1:45000", apiTarget)
	}
	webTarget, ok := m.Proxy.Lookup("web.tier1-c.grove.local")
	if !ok {
		t.Fatalf("web route not restored from state.json")
	}
	if webTarget != "127.0.0.1:45001" {
		t.Errorf("web target = %q, want 127.0.0.1:45001", webTarget)
	}
}

// TestWriteStateFile_PersistsProxyRoutes verifies the Up -> state.json path
// propagates resp.ProxyRoutes so the global daemon can rebuild routes from
// disk after a restart.
func TestWriteStateFile_PersistsProxyRoutes(t *testing.T) {
	tmp := t.TempDir()
	wtPath := filepath.Join(tmp, "tier1-c")
	stateDir := filepath.Join(wtPath, ".grove", "env")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	m := NewManager()
	m.envs["tier1-c"] = &RunningEnv{
		Provider: "docker",
		Worktree: "tier1-c",
		StateDir: stateDir,
		Ports:    map[string]int{"api": 45000},
	}

	req := coreenv.EnvRequest{
		Provider:  "docker",
		StateDir:  stateDir,
		Workspace: &workspace.WorkspaceNode{Name: "tier1-c", Path: wtPath},
	}
	resp := &coreenv.EnvResponse{
		Status:      "running",
		ProxyRoutes: map[string]int{"api": 45000},
	}

	if err := m.writeStateFile(t.Context(), req, resp); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got coreenv.EnvStateFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ProxyRoutes["api"] != 45000 {
		t.Errorf("persisted ProxyRoutes[api] = %d, want 45000", got.ProxyRoutes["api"])
	}
}
