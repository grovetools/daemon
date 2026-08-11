package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/coderoot"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/subject"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/core/util/pathutil"
)

// SetNotespaceAdopted wires the sync watcher notification that runs only after
// the stamp and machine primary are durable. Registration/pipeline creation
// therefore cannot race ahead of identity materialization.
func (s *Server) SetNotespaceAdopted(notify func(root, displayName string)) {
	s.notespaceAdopted = notify
}

func (s *Server) handleNotespaceAdopt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req models.NotespaceAdoption
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid adoption signal: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := materializeAdoptedNotespace(req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errAdoptionOutsideScanRoot) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return
	}
	if s.notespaceAdopted != nil {
		s.notespaceAdopted(result.NotespaceRoot, req.DisplayName)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

var errAdoptionOutsideScanRoot = errors.New("adopted checkout is not under an enabled recorded scan root")

func materializeAdoptedNotespace(req models.NotespaceAdoption) (*models.NotespaceAdoptionResult, error) {
	if req.Kind != "repo" && req.Kind != "ecosystem" {
		return nil, fmt.Errorf("adoption kind %q is not repo or ecosystem", req.Kind)
	}
	if req.DisplayName == "" || strings.TrimSpace(req.DisplayName) != req.DisplayName ||
		req.DisplayName == "." || req.DisplayName == ".." || strings.ContainsAny(req.DisplayName, "/\\\r\n\t") {
		return nil, fmt.Errorf("invalid adoption display name %q", req.DisplayName)
	}
	if err := subject.Validate(req.Subject); err != nil {
		return nil, fmt.Errorf("invalid canonical adoption subject: %w", err)
	}
	if !filepath.IsAbs(req.Root) || filepath.Clean(req.Root) != req.Root {
		return nil, fmt.Errorf("adoption root must be a clean absolute path")
	}
	root, err := pathutil.CanonicalPath(req.Root)
	if err != nil {
		return nil, fmt.Errorf("canonicalize adopted checkout: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("adopted checkout %s is not a directory", root)
	}
	if req.Kind == "repo" {
		if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
			return nil, fmt.Errorf("adopted repo %s has no .git marker", root)
		}
	} else if config.FindEcosystemManifest(root) == "" {
		return nil, fmt.Errorf("adopted ecosystem %s has no grove manifest", root)
	}

	table, err := coderoot.Load()
	if err != nil {
		return nil, fmt.Errorf("load recorded roots: %w", err)
	}
	rootName, ok := containingScanRoot(table, root)
	if !ok {
		return nil, errAdoptionOutsideScanRoot
	}
	notebookRoot := table.NotebookRoot(table.RootNotebook(rootName))
	if notebookRoot == "" {
		return nil, fmt.Errorf("scan root %q has no recorded notebook root", rootName)
	}
	notebookRoot, err = filepath.Abs(notebookRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve notebook root: %w", err)
	}
	if err := workspace.ValidateNotespaceLayout(notebookRoot); err != nil {
		return nil, err
	}
	notespaceRoot := filepath.Join(notebookRoot, workspace.NotespaceDirectory, req.DisplayName)

	// Every trust check above precedes the first mutation. In particular, a
	// missing stamp discovered by an ordinary scan never reaches this function.
	if err := os.MkdirAll(notespaceRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create adopted notespace root: %w", err)
	}
	before, err := notespace.LoadNotespace(notespaceRoot)
	if err != nil {
		return nil, err // malformed-is-error, never replace
	}
	stamp, err := notespace.MintNotespace(notespaceRoot, notespace.NotespaceMutable{
		Name: req.DisplayName, Subject: req.Subject, Kind: req.Kind,
	})
	if err != nil {
		return nil, err
	}
	if stamp.Subject != req.Subject || stamp.Kind != req.Kind {
		return nil, fmt.Errorf("existing notespace stamp at %s belongs to subject %q kind %q", notespaceRoot, stamp.Subject, stamp.Kind)
	}
	_, _, err = config.EditMachineConfig(config.MachineConfigPath(), config.MachineEditOptions{}, func(machine *config.MachineConfig) error {
		if machine.Primaries == nil {
			machine.Primaries = map[string]string{}
		}
		if machine.Primaries[req.Subject] == "" {
			machine.Primaries[req.Subject] = stamp.ID
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("record adopted notespace primary: %w", err)
	}
	return &models.NotespaceAdoptionResult{
		NotespaceID: stamp.ID, NotespaceRoot: notespaceRoot, Minted: before == nil,
	}, nil
}

func containingScanRoot(table coderoot.Table, adopted string) (string, bool) {
	// Deepest-root selection is the machine routing contract. Compare physical
	// paths so macOS /var -> /private/var aliases cannot defeat containment.
	name, root, bestLen := "", "", -1
	for candidate, declared := range table.Roots {
		if declared.Enabled != nil && !*declared.Enabled {
			continue
		}
		expanded := expandHome(os.ExpandEnv(declared.Path))
		abs, err := filepath.Abs(expanded)
		if err != nil {
			continue
		}
		physical, err := pathutil.CanonicalPath(abs)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(physical, adopted)
		if err != nil || (rel != "." && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			continue
		}
		if len(physical) > bestLen {
			name, root, bestLen = candidate, physical, len(physical)
		}
	}
	declared, ok := table.Roots[name]
	if !ok || !declared.Scan || !declared.IncludesRepo(filepath.Base(adopted)) {
		return "", false
	}
	rel, err := filepath.Rel(root, adopted)
	if err != nil || rel == "." {
		return "", false
	}
	depth := len(strings.Split(rel, string(filepath.Separator)))
	if declared.Depth != nil && depth > *declared.Depth {
		return "", false
	}
	return name, true
}
