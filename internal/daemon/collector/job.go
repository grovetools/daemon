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
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/sirupsen/logrus"
)

// JobCollector scans the filesystem for idle/pending jobs and seeds the daemon store.
// This ensures jobs created by `flow chat` (which only writes a markdown file)
// are visible in the daemon's ListJobs API and the hooks TUI.
type JobCollector struct {
	interval time.Duration
}

// NewJobCollector creates a new JobCollector with the specified interval.
func NewJobCollector(interval time.Duration) *JobCollector {
	if interval == 0 {
		interval = 5 * time.Minute
	}
	return &JobCollector{interval: interval}
}

func (c *JobCollector) Name() string { return "job" }

func (c *JobCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	logger := logging.NewLogger("collector.job")
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	scan := func() {
		jobs := discoverJobsFromFilesystem(logger)
		if len(jobs) > 0 {
			updates <- store.Update{
				Type:    store.UpdateJobsDiscovered,
				Source:  "job_collector",
				Scanned: len(jobs),
				Payload: jobs,
			}
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
func discoverJobsFromFilesystem(logger *logrus.Entry) []*models.JobInfo {
	discoveryLogger := logrus.New()
	discoveryLogger.SetLevel(logrus.WarnLevel)
	discoveryService := workspace.NewDiscoveryService(discoveryLogger)
	discoveryResult, err := discoveryService.DiscoverAll()
	if err != nil {
		logger.WithError(err).Error("Workspace discovery failed")
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
		logger.WithError(err).Error("Failed to scan for plans")
		return nil
	}

	var discoveredJobs []*models.JobInfo

	// Build a lookup map from workspace name -> node for worktree resolution
	allNodes := provider.All()
	nodesByName := make(map[string]*workspace.WorkspaceNode, len(allNodes))
	for _, n := range allNodes {
		nodesByName[n.Name] = n
	}

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
				file, err := os.Open(jobPath)
				if err != nil {
					continue
				}

				meta, err := frontmatter.Parse(file)
				file.Close()
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

				// Resolve workspace: if frontmatter specifies a worktree, try to find
				// the matching workspace node for accurate WorkDir
				jobWorkDir := ownerWorkDir
				jobRepo := ownerRepo
				jobBranch := ""
				if meta.Worktree != "" {
					jobBranch = meta.Worktree
					if wtNode, ok := nodesByName[meta.Worktree]; ok {
						jobWorkDir = wtNode.Path
						jobRepo = wtNode.Name
					}
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
					SubmittedAt: submittedAt,
				}
				discoveredJobs = append(discoveredJobs, job)
			}
		}
	}

	if len(discoveredJobs) > 0 {
		logger.WithField("count", len(discoveredJobs)).Debug("Discovered jobs from filesystem")
	}

	return discoveredJobs
}
