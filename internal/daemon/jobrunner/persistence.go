package jobrunner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
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
	os.MkdirAll(dir, 0755)
	return &Persistence{dir: dir}
}

// Save persists a job's state to disk as JSON.
func (p *Persistence) Save(job *models.JobInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(p.dir, job.ID+".json"), b, 0644)
}

// Load reads all persisted job files from disk.
func (p *Persistence) Load() []*models.JobInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
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
