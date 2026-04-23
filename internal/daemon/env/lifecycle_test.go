package env

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
)

func TestRunPreStopHook_TouchesSentinel(t *testing.T) {
	tmp := t.TempDir()
	sentinel := filepath.Join(tmp, "sentinel")

	m := NewManager()
	req := coreenv.EnvRequest{
		Workspace: &workspace.WorkspaceNode{Name: "test-prestop", Path: tmp},
		StateDir:  filepath.Join(tmp, ".grove", "env"),
		Config: map[string]interface{}{
			"lifecycle": map[string]interface{}{
				"pre_stop": "touch " + sentinel,
			},
		},
	}

	m.runPreStopHook(context.Background(), req, tmp, req.StateDir, nil)

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
}

func TestRunPreStopHook_NoOpWhenMissing(t *testing.T) {
	m := NewManager()
	req := coreenv.EnvRequest{
		Workspace: &workspace.WorkspaceNode{Name: "test-noprestop", Path: t.TempDir()},
		Config:    map[string]interface{}{},
	}
	// Should not panic or error.
	m.runPreStopHook(context.Background(), req, req.Workspace.Path, "", nil)
}

func TestRunPreStopHook_FailureDoesNotPanic(t *testing.T) {
	m := NewManager()
	req := coreenv.EnvRequest{
		Workspace: &workspace.WorkspaceNode{Name: "test-prestop-fail", Path: t.TempDir()},
		Config: map[string]interface{}{
			"lifecycle": map[string]interface{}{
				"pre_stop": "exit 7",
			},
		},
	}
	m.runPreStopHook(context.Background(), req, req.Workspace.Path, "", nil)
}

func TestRunStartupCommands_OnlyFlaggedRuns(t *testing.T) {
	tmp := t.TempDir()
	flagged := filepath.Join(tmp, "ran-flagged")
	unflagged := filepath.Join(tmp, "ran-unflagged")

	m := NewManager()
	req := coreenv.EnvRequest{
		Workspace: &workspace.WorkspaceNode{Name: "test-startup", Path: tmp},
		StateDir:  filepath.Join(tmp, ".grove", "env"),
		Config: map[string]interface{}{
			"commands": map[string]interface{}{
				"flagged": map[string]interface{}{
					"command": "touch " + flagged,
					"startup": true,
				},
				"unflagged": map[string]interface{}{
					"command": "touch " + unflagged,
				},
			},
		},
	}

	m.runStartupCommands(context.Background(), req, tmp, req.StateDir, nil)

	if _, err := os.Stat(flagged); err != nil {
		t.Errorf("flagged command did not run: %v", err)
	}
	if _, err := os.Stat(unflagged); err == nil {
		t.Errorf("unflagged command should not have run")
	}
}
