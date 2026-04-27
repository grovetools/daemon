package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/util/pathutil"
)

const taskResultsFile = "task-results.json"

// WorkspaceResults holds persisted task and test data for a single workspace.
type WorkspaceResults struct {
	TaskResults map[string]*models.TaskResult `json:"task_results,omitempty"`
	TestReports map[string]*models.TestReport `json:"test_reports,omitempty"`
}

// persistedState is the on-disk format: normalized workspace path -> results.
type persistedState map[string]*WorkspaceResults

// Persister handles async write-through of task results to disk.
type Persister struct {
	mu   sync.Mutex
	path string
}

func newPersister() *Persister {
	dir := paths.StateDir()
	if dir == "" {
		return &Persister{}
	}
	return &Persister{path: filepath.Join(dir, "daemon", taskResultsFile)}
}

// load reads persisted task results from disk. Returns nil on missing file.
func (p *Persister) load() (persistedState, error) {
	if p.path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return state, nil
}

// save writes the full state to disk. Called asynchronously.
func (p *Persister) save(state persistedState) {
	if p.path == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.path), 0o755)
	_ = os.WriteFile(p.path, data, 0o644)
}

// snapshot builds the persistable state from the current in-memory workspaces.
// Must be called under the store's read lock.
func snapshot(workspaces map[string]*models.EnrichedWorkspace) persistedState {
	state := make(persistedState)
	for _, ws := range workspaces {
		if len(ws.TaskResults) == 0 && len(ws.TestReports) == 0 {
			continue
		}
		key, err := pathutil.NormalizeForLookup(ws.Path)
		if err != nil {
			continue
		}
		state[key] = &WorkspaceResults{
			TaskResults: ws.TaskResults,
			TestReports: ws.TestReports,
		}
	}
	return state
}

// restoreResults populates task results and test reports on matching workspaces
// from persisted state. Must be called under the store's write lock.
func restoreResults(workspaces map[string]*models.EnrichedWorkspace, persisted persistedState) {
	if len(persisted) == 0 {
		return
	}
	for _, ws := range workspaces {
		if ws.WorkspaceNode == nil {
			continue
		}
		key, err := pathutil.NormalizeForLookup(ws.Path)
		if err != nil {
			continue
		}
		if results, ok := persisted[key]; ok {
			if len(results.TaskResults) > 0 {
				ws.TaskResults = results.TaskResults
			}
			if len(results.TestReports) > 0 {
				ws.TestReports = results.TestReports
			}
		}
	}
}
