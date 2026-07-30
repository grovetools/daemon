package jobrunner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/util/frontmatter"
)

// Persistence handles saving and loading job state to disk for restart recovery.
type Persistence struct {
	dir string
	mu  sync.Mutex
}

// NewPersistence creates a Persistence instance using the default state directory.
func NewPersistence() *Persistence {
	return NewPersistenceWithDir("")
}

// NewPersistenceWithDir creates a Persistence instance with a custom directory.
// If dir is empty, defaults to ~/.local/state/grove/daemon/jobs/.
func NewPersistenceWithDir(dir string) *Persistence {
	if dir == "" {
		dir = filepath.Join(paths.StateDir(), "daemon", "jobs")
	}
	_ = os.MkdirAll(dir, 0o755) //nolint:gosec // G301: daemon state directory
	return &Persistence{dir: dir}
}

// Save persists a job's state to disk as JSON.
func (p *Persistence) Save(job *models.JobInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked(job)
}

func (p *Persistence) saveLocked(job *models.JobInfo) {
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(p.dir, job.ID+".json"), b, 0o644) //nolint:gosec // G306: daemon state file
}

// Get reads a single persisted job record by ID, or nil when this daemon has
// never written one (or the file is unreadable/corrupt). Callers that need one
// known job must use this rather than filtering Load(): the state directory
// accumulates a file per job ever submitted, so a full load is unbounded work
// for a single lookup.
func (p *Persistence) Get(jobID string) *models.JobInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	b, err := os.ReadFile(filepath.Join(p.dir, jobID+".json")) //nolint:gosec // G304: ID-keyed daemon state file
	if err != nil {
		return nil
	}
	var j models.JobInfo
	if json.Unmarshal(b, &j) != nil {
		return nil
	}
	return &j
}

// Load reads all persisted job files from disk.
func (p *Persistence) Load() []*models.JobInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadLocked()
}

func (p *Persistence) loadLocked() []*models.JobInfo {
	var jobs []*models.JobInfo
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p.dir, e.Name()))
		if err != nil {
			continue
		}
		var j models.JobInfo
		if json.Unmarshal(b, &j) == nil {
			jobs = append(jobs, &j)
		}
	}
	return jobs
}

// Remove deletes a persisted job file from disk.
func (p *Persistence) Remove(jobID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = os.Remove(filepath.Join(p.dir, jobID+".json"))
}

// CollapseDuplicates merges legacy filename-keyed job records into the
// Flow-ID-keyed record for the same job and removes the duplicate file. It
// returns (merged, removed).
//
// Submissions used to mint their own key from the job filename, so one job
// could own two records: the Flow-ID one (typed, artifact paths keyed by the
// Flow ID) and a filename-keyed one carrying no type at all. Consumers that
// branch on record shape then answered from whichever record they happened to
// win — that is how `aglogs` ended up resolving a job.log instead of a
// transcript — and restart recovery evaluated the same job twice.
//
// Submit no longer creates filename-keyed records; this migrates the ones
// already on disk. A filename-keyed record with no Flow-ID partner is rekeyed
// from its job file's frontmatter when that file is still readable, and is left
// alone otherwise — an unresolvable record is stale history, not something to
// guess an identity for.
func (p *Persistence) CollapseDuplicates() (merged, removed int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	jobs := p.loadLocked()

	// Index by (plan dir, job file) — the pair that identifies one job
	// independent of which key its record was written under.
	type groupKey struct{ planDir, jobFile string }
	groups := make(map[groupKey][]*models.JobInfo, len(jobs))
	for _, job := range jobs {
		if job.PlanDir == "" || job.JobFile == "" {
			continue
		}
		key := groupKey{planDir: filepath.Clean(job.PlanDir), jobFile: job.JobFile}
		groups[key] = append(groups[key], job)
	}

	for key, group := range groups {
		var canonical *models.JobInfo
		var dupes []*models.JobInfo
		for _, job := range group {
			if isFilenameKeyed(job.ID, job.JobFile) {
				dupes = append(dupes, job)
				continue
			}
			// Prefer a typed record as canonical; among typed ones the first
			// wins (there is only ever one Flow ID per job file).
			if canonical == nil || (canonical.Type == "" && job.Type != "") {
				canonical = job
			}
		}
		if len(dupes) == 0 {
			continue
		}

		if canonical == nil {
			// No Flow-ID record on disk. Recover the identity from the job
			// file itself; skip the record when the plan is gone.
			flowID, flowType := jobIdentityFromFile(key.planDir, key.jobFile)
			if flowID == "" || flowID == dupes[0].ID {
				continue
			}
			promoted := *dupes[0]
			promoted.ID = flowID
			if promoted.Type == "" {
				promoted.Type = models.JobType(flowType)
			}
			canonical = &promoted
			p.saveLocked(canonical)
			merged++
		}

		for _, dupe := range dupes {
			mergeJobRecord(canonical, dupe)
			_ = os.Remove(filepath.Join(p.dir, dupe.ID+".json"))
			removed++
		}
		if canonical.Type == "" {
			_, flowType := jobIdentityFromFile(key.planDir, key.jobFile)
			canonical.Type = models.JobType(flowType)
		}
		p.saveLocked(canonical)
		merged++
	}

	return merged, removed
}

// isFilenameKeyed reports whether an ID was minted from a job filename rather
// than taken from the job's frontmatter — the "<job-file-base>-<6 hex>" shape
// Submit used to generate.
func isFilenameKeyed(id, jobFile string) bool {
	base := strings.TrimSuffix(jobFile, ".md")
	if base == "" || id == "" {
		return false
	}
	suffix, ok := strings.CutPrefix(id, base+"-")
	return ok && len(suffix) == 6
}

// mergeJobRecord copies fields the duplicate knows about and the canonical
// record lacks. It never overwrites populated canonical fields: the Flow-ID
// record is the authority on identity and lifecycle; the duplicate only
// contributes launch details the submission path recorded.
func mergeJobRecord(canonical, dupe *models.JobInfo) {
	if canonical == nil || dupe == nil {
		return
	}
	if canonical.LogFilePath == "" {
		canonical.LogFilePath = dupe.LogFilePath
	}
	if canonical.AgentTarget == "" {
		canonical.AgentTarget = dupe.AgentTarget
	}
	if canonical.PID == 0 {
		canonical.PID = dupe.PID
	}
	if canonical.StartedAt == nil {
		canonical.StartedAt = dupe.StartedAt
	}
	if canonical.CompletedAt == nil {
		canonical.CompletedAt = dupe.CompletedAt
	}
	if canonical.TimeoutStr == "" {
		canonical.TimeoutStr = dupe.TimeoutStr
	}
	if len(canonical.Env) == 0 {
		canonical.Env = dupe.Env
	}
}

// jobIdentityFromFile reads a job file's frontmatter and returns its Flow job
// ID and type. Both are empty when the file is unreadable.
func jobIdentityFromFile(planDir, jobFile string) (id, jobType string) {
	f, err := os.Open(filepath.Join(planDir, jobFile))
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	meta, err := frontmatter.Parse(f)
	if err != nil {
		return "", ""
	}
	return meta.ID, meta.Type
}
