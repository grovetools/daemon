package jobrunner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/sirupsen/logrus"
)

// materializeBundle writes a shipped plan bundle onto this node's replica plan
// directory hash-idempotently and returns the plan dir (M2 C12). The written
// files persist on disk (LoadPlan re-stats them at dispatch); they are ordinary
// replica-notebook content thereafter and converge home via M1 sync (C13).
func (jr *JobRunner) materializeBundle(ctx context.Context, bundle *models.PlanBundle) (string, error) {
	planDir, err := resolveReplicaPlanDir(bundle.Workspace, bundle.PlanName)
	if err != nil {
		return "", err
	}
	if err := jr.writeBundleFiles(planDir, bundle); err != nil {
		return "", err
	}
	jr.ulog.Info("Materialized plan bundle").
		Field("workspace", bundle.Workspace).
		Field("plan", bundle.PlanName).
		Field("plan_dir", planDir).
		Field("files", len(bundle.Files)).
		Log(ctx)
	return planDir, nil
}

// resolveReplicaPlanDir resolves <replica notebook>/…/plans/<planName> for the
// named workspace (M2 C12/D2). It looks the workspace up in discovery to get a
// node carrying the correct notebook binding; failing that it falls back to a
// bare node, which still renders the right path for the default centralized
// notebook (workspaces/<name>/plans).
func resolveReplicaPlanDir(workspaceName, planName string) (string, error) {
	if workspaceName == "" {
		return "", fmt.Errorf("plan bundle missing workspace name")
	}
	if planName == "" {
		return "", fmt.Errorf("plan bundle missing plan name")
	}
	cfg, _ := config.LoadDefault() // NewNotebookLocator tolerates a nil config
	node := lookupWorkspaceNode(workspaceName)
	if node == nil {
		node = &workspace.WorkspaceNode{Name: workspaceName}
	}
	plansDir, err := workspace.NewNotebookLocator(cfg).GetPlansDir(node)
	if err != nil {
		return "", fmt.Errorf("resolving replica plans dir for %q: %w", workspaceName, err)
	}
	return filepath.Join(plansDir, planName), nil
}

// lookupWorkspaceNode resolves a workspace name to its discovered node, or nil.
func lookupWorkspaceNode(name string) *workspace.WorkspaceNode {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	nodes, err := workspace.GetProjects(logger)
	if err != nil {
		return nil
	}
	return workspace.NewProviderFromNodes(nodes).FindByName(name)
}

// writeBundleFiles materializes bundle.Files under planDir hash-idempotently:
// byte-identical files are skipped, path-traversal keys are rejected, and a file
// that differs is refused (whole submit) when a running job in this plan dir is
// using it (M2 C12/D3). Dir/file modes match flow's SavePlan (0o755 / 0o600).
func (jr *JobRunner) writeBundleFiles(planDir string, bundle *models.PlanBundle) error {
	absPlanDir, err := filepath.Abs(planDir)
	if err != nil {
		return fmt.Errorf("absolutizing plan dir: %w", err)
	}

	runningFiles := jr.runningJobFiles(absPlanDir)

	if err := os.MkdirAll(absPlanDir, 0o755); err != nil {
		return fmt.Errorf("creating replica plan dir: %w", err)
	}

	for key, content := range bundle.Files {
		clean := filepath.FromSlash(key)
		if !filepath.IsLocal(clean) {
			return fmt.Errorf("rejecting non-local bundle path %q", key)
		}
		dest := filepath.Join(absPlanDir, clean)

		if existing, readErr := os.ReadFile(dest); readErr == nil {
			if sha256.Sum256(existing) == sha256.Sum256(content) {
				continue // byte-identical — idempotent skip
			}
			if runningFiles[clean] {
				return fmt.Errorf("refusing to overwrite %q: a running job in this plan is using it", key)
			}
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("creating dir for %q: %w", key, err)
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return fmt.Errorf("writing %q: %w", key, err)
		}
	}
	return nil
}

// runningJobFiles returns the set of plan-dir-relative job files for jobs
// currently running in planDir (absolute), used to refuse a mutating overwrite.
func (jr *JobRunner) runningJobFiles(planDir string) map[string]bool {
	out := map[string]bool{}
	if jr.store == nil {
		return out
	}
	for _, j := range jr.store.GetJobs() {
		if j.Status != "running" {
			continue
		}
		jp := j.PlanDir
		if abs, err := filepath.Abs(jp); err == nil {
			jp = abs
		}
		if jp == planDir {
			out[filepath.FromSlash(j.JobFile)] = true
		}
	}
	return out
}
