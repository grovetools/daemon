// Machine identity + intent HTTP handler — GET /api/machine. Like the sync
// routes this is served on the 0600 unix socket only: it reports this host's
// identity and the paths it expects its ecosystems at, which is inventory, not
// public data.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/coderoot"
	"github.com/grovetools/core/pkg/machine"
)

// machineStatusResponse is the GET /api/machine payload. models.MachineStatus
// is its client-side mirror.
type machineStatusResponse struct {
	Name       string                  `json:"name"`
	ID         string                  `json:"id,omitempty"`
	ConfigPath string                  `json:"config_path,omitempty"`
	Ecosystems []machineEcosystemState `json:"ecosystems,omitempty"`
	Roots      []machineRootState      `json:"roots,omitempty"`
}

type machineEcosystemState struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Notebook string `json:"notebook,omitempty"`
	State    string `json:"state"`
	Manifest string `json:"manifest,omitempty"`
	Enabled  bool   `json:"enabled"`
}

type machineRootState struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Notebook string `json:"notebook,omitempty"`
	Enabled  bool   `json:"enabled"`
	Exists   bool   `json:"exists"`
}

// handleMachineStatus handles GET /api/machine: this machine's identity plus
// its declared intent reconciled against the disk.
//
// The reconciliation is computed here, during the daemon's own view of
// discovery, so "declared but missing" has a live answer without every caller
// re-statting the filesystem. P6 feeds the same states into the machine note
// (state = declared-missing) so peers can see the gap too.
//
// A machine with no recorded roots is not an error: the response is just the
// name (hostname default) and the id, with no declared code intent.
func (s *Server) handleMachineStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	out := machineStatusResponse{
		Name:       config.ResolveMachineName(),
		ID:         machine.ID(),
		ConfigPath: config.MachineConfigPath(),
	}

	codeRoots, err := coderoot.Load()
	if err != nil {
		// Unreadable recorded routing is a real operator problem, but identity
		// still answers — report what is knowable rather than 500ing the
		// whole surface.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(out)
		return
	}

	for _, st := range config.ReconcileCodeRoots(codeRoots) {
		out.Ecosystems = append(out.Ecosystems, machineEcosystemState{
			Name:     st.Name,
			Path:     st.Path,
			Notebook: st.Notebook,
			State:    st.State,
			Manifest: st.Manifest,
			Enabled:  st.Enabled,
		})
	}
	for _, name := range codeRoots.SortedRootNames() {
		root := codeRoots.Roots[name]
		if !root.Scan {
			continue
		}
		path := os.ExpandEnv(root.Path)
		if abs, err := filepath.Abs(expandHome(path)); err == nil {
			path = abs
		}
		info, statErr := os.Stat(path)
		out.Roots = append(out.Roots, machineRootState{
			Name:     name,
			Path:     path,
			Notebook: codeRoots.RootNotebook(name),
			Enabled:  root.Enabled == nil || *root.Enabled,
			Exists:   statErr == nil && info.IsDir(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// expandHome expands a leading ~/ the way core's config loader does.
func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
