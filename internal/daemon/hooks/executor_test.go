package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/config"
)

// These tests cover the HookCommand fields core has always declared and the
// daemon's executor always ignored: a hook that set timeout = 300 was killed
// at 30 seconds anyway, cancel_previous did nothing, and a hook gated behind
// enable_env ran unconditionally.

func newTestExecutor() *Executor { return NewExecutor(&config.Config{}) }

func TestExecuteHookWritesStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin.txt")
	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "cat", Command: "cat > " + out},
		HookRun{Stdin: []byte("hello from the bus")})

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook did not run: %v", err)
	}
	if string(data) != "hello from the bus" {
		t.Fatalf("stdin = %q", data)
	}
}

func TestExecuteHookHonorsTheConfiguredTimeout(t *testing.T) {
	out := filepath.Join(t.TempDir(), "done.txt")
	start := time.Now()
	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "slow", Command: "sleep 5; touch " + out, Timeout: 1},
		HookRun{})
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("hook ran for %s; timeout = 1 was not honored", elapsed)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("the hook completed despite its timeout")
	}
}

func TestExecuteHookDefaultTimeoutIsThirtySeconds(t *testing.T) {
	// Asserted on the constant rather than by waiting: the point is that the
	// previously hard-coded 30s became the DEFAULT, so every existing
	// on_skill_sync config keeps its behavior.
	if DefaultHookTimeout != 30*time.Second {
		t.Fatalf("DefaultHookTimeout = %s, want 30s (the skill-sync executor's historical hard-coded value)", DefaultHookTimeout)
	}
}

func TestExecuteHookDisableEnvSkips(t *testing.T) {
	out := filepath.Join(t.TempDir(), "ran.txt")
	t.Setenv("GROVE_TEST_MUTE", "1")

	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "muted", Command: "touch " + out, DisableEnv: "GROVE_TEST_MUTE"},
		HookRun{})
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a hook gated by disable_env ran while the variable was set")
	}

	t.Setenv("GROVE_TEST_MUTE", "")
	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "muted", Command: "touch " + out, DisableEnv: "GROVE_TEST_MUTE"},
		HookRun{})
	if _, err := os.Stat(out); err != nil {
		t.Fatal("a hook stayed muted after its disable_env variable was cleared")
	}
}

func TestExecuteHookEnableEnvGates(t *testing.T) {
	out := filepath.Join(t.TempDir(), "ran.txt")
	t.Setenv("GROVE_TEST_ARM", "")

	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "opt-in", Command: "touch " + out, EnableEnv: "GROVE_TEST_ARM"},
		HookRun{})
	if _, err := os.Stat(out); err == nil {
		t.Fatal("an enable_env hook ran without being armed")
	}

	t.Setenv("GROVE_TEST_ARM", "yes")
	newTestExecutor().ExecuteHook(context.Background(),
		config.HookCommand{Name: "opt-in", Command: "touch " + out, EnableEnv: "GROVE_TEST_ARM"},
		HookRun{})
	if _, err := os.Stat(out); err != nil {
		t.Fatal("an armed enable_env hook did not run")
	}
}

// cancel_previous is what keeps a fast-firing event (a job stream, a note
// watcher) from stacking up N copies of a slow hook.
func TestExecuteHookCancelPrevious(t *testing.T) {
	dir := t.TempDir()
	e := newTestExecutor()
	hook := config.HookCommand{
		Name:           "slow",
		Command:        "sleep 3; touch " + filepath.Join(dir, "finished-$GROVE_RUN"),
		Timeout:        10,
		CancelPrevious: true,
	}

	done := make(chan struct{})
	go func() {
		e.ExecuteHook(context.Background(), hook, HookRun{Env: map[string]string{"GROVE_RUN": "first"}})
		close(done)
	}()

	// Wait for the first run to be tracked, then supersede it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		tracked := len(e.running)
		e.mu.Unlock()
		if tracked > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	go e.ExecuteHook(context.Background(), hook, HookRun{Env: map[string]string{"GROVE_RUN": "second"}})

	select {
	case <-done:
		// The first run must have been killed well before its sleep finished.
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("the first run was not cancelled when a newer event fired")
	}

	if _, err := os.Stat(filepath.Join(dir, "finished-first")); err == nil {
		t.Fatal("the cancelled run completed anyway")
	}
}

// Without cancel_previous, concurrent events must both run to completion.
func TestExecuteHookWithoutCancelPreviousRunsBoth(t *testing.T) {
	dir := t.TempDir()
	e := newTestExecutor()
	hook := config.HookCommand{Name: "quick", Command: "touch " + filepath.Join(dir, "run-$GROVE_RUN"), Timeout: 10}

	for _, id := range []string{"a", "b"} {
		e.ExecuteHook(context.Background(), hook, HookRun{Env: map[string]string{"GROVE_RUN": id}})
	}
	for _, id := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(dir, "run-"+id)); err != nil {
			t.Errorf("run %s did not complete: %v", id, err)
		}
	}
	// Nothing is tracked when cancel_previous is off — tracking every hook
	// would make the map grow with the daemon's uptime.
	e.mu.Lock()
	tracked := len(e.running)
	e.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d hooks tracked without cancel_previous", tracked)
	}
}

func TestExecuteOnSkillSyncStillHonorsRunIf(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "synced.txt")
	cfg := &config.Config{Daemon: &config.DaemonConfig{Hooks: &config.DaemonHooks{
		OnSkillSync: []config.HookCommand{{
			Name: "on-change", Command: "echo \"$GROVE_SYNCED_SKILLS\" > " + out, RunIf: "changes",
		}},
	}}}
	e := NewExecutor(cfg)

	e.ExecuteOnSkillSync(context.Background(), "/ws", nil, false)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a run_if=changes hook ran when nothing changed")
	}

	e.ExecuteOnSkillSync(context.Background(), "/ws", []string{"alpha", "beta"}, true)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the hook did not run on a change: %v", err)
	}
	if strings.TrimSpace(string(data)) != "alpha,beta" {
		t.Fatalf("GROVE_SYNCED_SKILLS = %q", data)
	}
}
