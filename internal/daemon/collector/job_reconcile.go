package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/sessions/health"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/flow/pkg/orchestration"
)

// The JobCollector's stuck-file sweep.
//
// The session reaper (session.go) covers jobs the daemon still tracks:
// when it kills a session it now reconciles that session's job file in
// the same breath. This covers the other half — jobs with NO daemon
// session at all. A daemon restart that lost a job, a process killed
// out from under everyone, an agent from before the reaper existed:
// all of them leave a .md file claiming "running" that nothing will
// ever correct, because there is no session left to reap.
//
// The sweep runs on the JobCollector's existing filesystem tick, so it
// costs one extra registry read per five minutes and no new scanning.
//
// It is deliberately hard to trigger. A file is only reconciled when
// every one of these holds:
//
//   - its frontmatter claims an active status (running/in_progress)
//   - a matching daemon row has authoritative Verified=stale, OR no row
//     exists and the shared classifier freshly finds stale
//   - the file itself has been untouched for QuietFor (default 10m)
//   - the job belongs to this daemon's scope
//
// The default applies these conservative loss corrections. The config retains
// a report-only off-switch. Success and unattended abandonment are forbidden.

const (
	defaultReconcileQuietFor  = 10 * time.Minute
	defaultReconcileMaxPerRun = 25
	defaultSessionRetention   = 14 * 24 * time.Hour
)

// reconcileSettings is the resolved, defaulted configuration.
type reconcileSettings struct {
	enabled          bool
	quietFor         time.Duration
	maxPerRun        int
	sessionRetention time.Duration
}

func resolveReconcileSettings(cfg *config.Config) reconcileSettings {
	s := reconcileSettings{
		enabled:          true,
		quietFor:         defaultReconcileQuietFor,
		maxPerRun:        defaultReconcileMaxPerRun,
		sessionRetention: defaultSessionRetention,
	}
	if cfg == nil || cfg.Daemon == nil {
		return s
	}
	if cfg.Daemon.SessionRetention != "" {
		if d, err := time.ParseDuration(cfg.Daemon.SessionRetention); err == nil && d > 0 {
			s.sessionRetention = d
		}
	}
	if cfg.Daemon.JobReconcile == nil {
		return s
	}
	jr := cfg.Daemon.JobReconcile
	if jr.Enabled != nil {
		s.enabled = *jr.Enabled
	}
	if jr.QuietFor != "" {
		if d, err := time.ParseDuration(jr.QuietFor); err == nil && d > 0 {
			s.quietFor = d
		}
	}
	if jr.MaxPerRun > 0 {
		s.maxPerRun = jr.MaxPerRun
	}
	return s
}

// reconcileCandidate is one job the sweep is considering, with the
// evidence that made it a candidate. Everything needed for the log line
// is captured here so a wrong flip can be reconstructed after the fact.
type reconcileCandidate struct {
	job       *models.JobInfo
	path      string
	from      string
	to        string
	quiet     time.Duration
	verdict   health.Verdict
	scopeSeen string
	source    string // stored_verdict | fresh_probe
}

func (c reconcileCandidate) evidence() string {
	return fmt.Sprintf("source=%s; %s; file untouched %s", c.source, c.verdict.Reason, health.RoundDur(c.quiet))
}

// sweepStuckJobFiles reconciles job files with no live process behind
// them. Returns the number of files it changed (always 0 in
// report-only mode).
func (c *JobCollector) sweepStuckJobFiles(
	ctx context.Context,
	ulog *logging.UnifiedLogger,
	st *store.Store,
	jobs []*models.JobInfo,
	now time.Time,
) int {
	settings := resolveReconcileSettings(c.cfg)

	// Index active rows. A present row is eligible only when Phase 2's
	// authoritative verdict is stale; unverified and empty remain vetoes.
	byFile := make(map[string]*models.Session)
	byID := make(map[string]*models.Session)
	for _, s := range st.GetSessions() {
		if s == nil || (!health.IsActiveSessionStatus(s.Status) && s.Status != "pending") {
			continue
		}
		if s.JobFilePath != "" {
			byFile[s.JobFilePath] = s
		}
		byID[s.ID] = s
	}

	candidates := c.collectCandidates(jobs, byFile, byID, settings, now)
	if len(candidates) == 0 {
		return 0
	}

	if len(candidates) > settings.maxPerRun {
		// Never silently truncate: a sweep that stops early while
		// reporting success reads as "everything is reconciled".
		ulog.Warn("Stuck job files exceed the per-run cap; deferring the rest to the next sweep").
			Field("found", len(candidates)).
			Field("cap", settings.maxPerRun).
			Log(ctx)
		candidates = candidates[:settings.maxPerRun]
	}

	changed := 0
	for _, cand := range candidates {
		if !settings.enabled {
			ulog.Info("Would reconcile unsupported active job claim (reporting only)").
				Field("event", "job.reconcile").
				Field("job_id", cand.job.ID).
				Field("job_file", cand.path).
				Field("observed", cand.evidence()).
				Field("concluded", cand.from+" -> "+cand.to).
				Field("changed", false).
				Field("hint", "remove daemon.job_reconcile.enabled=false to apply").
				StructuredOnly().Log(ctx)
			continue
		}
		didChange, err := orchestration.ReconcileJobFile(cand.path, cand.to)
		switch {
		case err != nil:
			ulog.Warn("Failed to reconcile unsupported active job claim").
				Err(err).
				Field("event", "job.reconcile").
				Field("job_id", cand.job.ID).
				Field("job_file", cand.path).
				Field("observed", cand.evidence()).
				Field("concluded", cand.from+" -> "+cand.to).
				Field("changed", false).
				StructuredOnly().Log(ctx)
		case didChange:
			changed++
			ulog.Info("Reconciled unsupported active job claim").
				Field("event", "job.reconcile").
				Field("job_id", cand.job.ID).
				Field("job_file", cand.path).
				Field("observed", cand.evidence()).
				Field("concluded", cand.from+" -> "+cand.to).
				Field("changed", true).
				StructuredOnly().Log(ctx)
		}
	}
	return changed
}

// collectCandidates applies every gate and returns the jobs that
// survived all of them. Split out from the sweep so the decision logic
// is testable without a store, a config or a filesystem write.
func (c *JobCollector) collectCandidates(
	jobs []*models.JobInfo,
	byFile, byID map[string]*models.Session,
	settings reconcileSettings,
	now time.Time,
) []reconcileCandidate {
	prober := &health.Prober{
		// No daemon client: this runs INSIDE the daemon, and the PTY
		// list is not reachable from here. That is safe in this
		// direction — a synthesized session carries no PtyID, so the
		// classifier never reads a missing PTY as proof of death; the
		// on-disk registry (which any live agent writes) is the signal
		// that actually decides these, and it needs no client.
		JobFile:  orchestration.ReadJobFileStatus,
		StateDir: c.stateDir, // empty == the real one; tests point it elsewhere
	}

	var out []reconcileCandidate
	for _, job := range jobs {
		if job == nil || job.PlanDir == "" || job.JobFile == "" {
			continue
		}
		if !health.IsJobFileStatusActive(job.Status) {
			continue
		}
		path := filepath.Join(job.PlanDir, job.JobFile)
		present := byFile[path]
		if present == nil {
			present = byID[job.ID]
		}
		if present != nil && present.Verified != "stale" {
			continue
		}

		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		quiet := now.Sub(info.ModTime())
		if quiet < settings.quietFor {
			continue
		}

		if present != nil {
			out = append(out, reconcileCandidate{
				job: job, path: path, from: job.Status,
				to: health.ReconciledStatusFor(string(job.Type)), quiet: quiet,
				verdict: health.Verdict{State: health.Stale, Reason: "daemon stored verified=stale"},
				source:  "stored_verdict",
			})
			continue
		}

		// No daemon row: synthesize the session this job claims to be, and put it
		// through the same ladder every other surface uses. Reusing the
		// classifier here is the point of promoting it to core: the
		// daemon's unattended sweep and the user's 'X' key convict on
		// identical evidence.
		synthetic := &models.Session{
			ID:           job.ID,
			Status:       "running",
			Type:         string(job.Type),
			JobFilePath:  path,
			PlanName:     job.PlanName,
			JobTitle:     job.Title,
			StartedAt:    job.SubmittedAt,
			LastActivity: info.ModTime(),
		}
		probe := prober.ProbeAt(context.Background(), []*models.Session{synthetic}, now)
		if len(probe) == 0 || probe[0].Verdict.State != health.Stale {
			continue
		}

		// Scope gate, exactly like the reaper's: only touch jobs whose
		// owning scope is ours. A registry record that names a
		// different scope belongs to another daemon; one that names
		// none is unscoped and only the unscoped daemon owns it.
		seenScope := probe[0].Evidence.MetaScope
		if probe[0].Evidence.HasMetadata && seenScope != c.scope {
			continue
		}

		out = append(out, reconcileCandidate{
			job:       job,
			path:      path,
			from:      job.Status,
			to:        health.ReconciledStatusFor(string(job.Type)),
			quiet:     quiet,
			verdict:   probe[0].Verdict,
			scopeSeen: seenScope,
			source:    "fresh_probe",
		})
	}
	return out
}
