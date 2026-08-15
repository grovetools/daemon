package collector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/frontmatter"
	"github.com/grovetools/daemon/internal/daemon/jobattr"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
	"github.com/sirupsen/logrus"
)

// JobCollector scans the filesystem for idle/pending jobs and seeds the daemon store.
// This ensures jobs created by `flow chat` (which only writes a markdown file)
// are visible in the daemon's ListJobs API and the hooks TUI.
//
// It also reconciles job files stuck on an active status with nothing
// alive behind them — see job_reconcile.go. That rides this collector's
// existing sweep because it already re-parses every job's frontmatter,
// so the extra work is one registry read per tick.
type JobCollector struct {
	interval time.Duration
	// scope is this daemon's owning scope ("" == unscoped/global). The
	// reconciliation sweep only touches jobs whose owning scope matches,
	// exactly like the session reaper, so a scoped daemon can never
	// rewrite another scope's job files.
	scope string
	// cfg supplies reconciliation and terminal-row retention settings.
	// Nil means conservative default-on loss correction and 14d retention.
	cfg *config.Config
	// stateDir overrides paths.StateDir() for the session-registry read
	// the reconciliation sweep does. Empty uses the real one; only
	// tests set it.
	stateDir string
}

// NewJobCollector creates a new JobCollector with the specified interval.
func NewJobCollector(interval time.Duration, scope string, cfg *config.Config) *JobCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &JobCollector{interval: interval, scope: scope, cfg: cfg}
}

func (c *JobCollector) Name() string { return "job" }

func (c *JobCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ulog := logging.NewUnifiedLogger("groved.collector.job")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	scan := func() {
		jobs := discoverJobsFromFilesystem(ctx, ulog)
		if len(jobs) > 0 {
			updates <- store.Update{
				Type:    store.UpdateJobsDiscovered,
				Source:  "job_collector",
				Scanned: len(jobs),
				Payload: jobs,
			}
		}
		// Reconcile job files claiming to run with nothing behind them.
		// Runs after the store update so the sweep sees the freshest
		// session picture it can.
		now := time.Now()
		c.sweepStuckJobFiles(ctx, ulog, st, jobs, now)

		settings := resolveReconcileSettings(c.cfg)
		cutoff := now.Add(-settings.sessionRetention)
		for _, id := range st.PruneTerminalSessions(cutoff, "job_reconcile") {
			telemetry.SessionTerminalRowsDropped.Inc()
			ulog.Info("Pruned retained terminal session row").
				Field("event", "session.retention_pruned").
				Field("job_id", id).
				Field("observed", "terminal row older than retention").
				Field("concluded", "in-memory history retention expired").
				Field("changed", true).
				Field("retention", settings.sessionRetention.String()).
				StructuredOnly().Log(ctx)
		}
	}

	// Wait for workspaces to be populated first
	time.Sleep(3 * time.Second)
	scan()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			scan()
		}
	}
}

// discoverJobsFromFilesystem scans all plan directories for job markdown files
// and returns JobInfo structs for each discovered job.
func discoverJobsFromFilesystem(ctx context.Context, ulog *logging.UnifiedLogger) []*models.JobInfo {
	discoveryLogger := logrus.New()
	discoveryLogger.SetLevel(logrus.WarnLevel)
	discoveryService := workspace.NewDiscoveryService(discoveryLogger)
	discoveryResult, err := discoveryService.DiscoverAll()
	if err != nil {
		ulog.Error("Workspace discovery failed").Err(err).Log(ctx)
		return nil
	}
	provider := workspace.NewProvider(discoveryResult)

	coreCfg, err := config.LoadDefault()
	if err != nil {
		coreCfg = &config.Config{}
	}
	locator := workspace.NewNotebookLocator(coreCfg)

	scannedDirs, err := locator.ScanForAllPlans(provider)
	if err != nil {
		ulog.Error("Failed to scan for plans").Err(err).Log(ctx)
		return nil
	}

	var discoveredJobs []*models.JobInfo

	index := jobattr.NewIndex(provider.All())

	for _, scannedDir := range scannedDirs {
		plansRootDir := scannedDir.Path

		// Derive workspace info from the ScannedDir owner
		var ownerWorkDir, ownerRepo string
		if scannedDir.Owner != nil {
			ownerWorkDir = scannedDir.Owner.Path
			ownerRepo = scannedDir.Owner.Name
		}

		entries, err := os.ReadDir(plansRootDir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			planPath := filepath.Join(plansRootDir, entry.Name())
			planName := entry.Name()
			jobEntries, err := os.ReadDir(planPath)
			if err != nil {
				continue
			}

			for _, jobEntry := range jobEntries {
				if jobEntry.IsDir() || !strings.HasSuffix(jobEntry.Name(), ".md") {
					continue
				}
				if jobEntry.Name() == "spec.md" || jobEntry.Name() == "README.md" {
					continue
				}

				jobPath := filepath.Join(planPath, jobEntry.Name())
				file, err := os.Open(jobPath) //nolint:gosec // G304: path from plan directory
				if err != nil {
					continue
				}

				meta, err := frontmatter.Parse(file)
				_ = file.Close()
				if err != nil {
					continue
				}

				if meta.ID == "" {
					continue
				}

				submittedAt := meta.StartedAt
				if submittedAt.IsZero() {
					submittedAt = meta.UpdatedAt
				}
				if submittedAt.IsZero() {
					if info, err := jobEntry.Info(); err == nil {
						submittedAt = info.ModTime()
					} else {
						submittedAt = time.Now()
					}
				}

				// Resolve workspace: if frontmatter specifies a worktree, find
				// the matching workspace node for an accurate WorkDir. The
				// lookup is constrained to the plan owner's own ecosystem —
				// see jobattr.Index.Resolve. The flow watcher publishes the
				// same rows from its fsnotify path and MUST use this same
				// helper, or the two producers overwrite each other.
				jobWorkDir, jobRepo, jobBranch, outcome := jobattr.JobWorkspace(
					index, scannedDir.Owner, meta.Worktree, ownerWorkDir, ownerRepo)
				if outcome == jobattr.Ambiguous {
					ulog.Warn("Ambiguous worktree name in job frontmatter; keeping owner-derived workspace").
						Field("job_path", jobPath).
						Field("worktree", meta.Worktree).
						Field("owner", ownerWorkDir).
						Log(ctx)
				}

				job := &models.JobInfo{
					ID:          meta.ID,
					Title:       meta.Title,
					Type:        models.JobType(meta.Type),
					Status:      meta.Status,
					PlanDir:     planPath,
					PlanName:    planName,
					JobFile:     jobEntry.Name(),
					WorkDir:     jobWorkDir,
					Repo:        jobRepo,
					Branch:      jobBranch,
					Channels:    meta.Channels,
					SubmittedAt: submittedAt,
				}
				discoveredJobs = append(discoveredJobs, job)
			}
		}
	}

	if len(discoveredJobs) > 0 {
		ulog.Debug("Discovered jobs from filesystem").Field("count", len(discoveredJobs)).Log(ctx)
	}

	return discoveredJobs
}
