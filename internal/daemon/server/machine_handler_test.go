package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// GET /api/machine answers identity + intent reconciled against the disk. It
// needs no sync.db and no daemon state: the whole answer is config plus a stat
// of each declared path.
func TestHandleMachineStatusReportsReconciledIntent(t *testing.T) {
	configDir := sandboxGroveHome(t)
	code := t.TempDir()

	present := filepath.Join(code, "grovetools")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(present, "grove.toml"), []byte("name = \"grovetools\"\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	chickens := filepath.Join(code, "chickens")
	if err := os.MkdirAll(chickens, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	machineTOML := `[machine]
name = "mbp"

[machine.ecosystems.grovetools]
path = "` + present + `"
notebook = "grovetools"

[machine.ecosystems.cloud]
path = "` + filepath.Join(code, "cloud") + `"

[machine.roots.chickens]
path = "` + chickens + `"
notebook = "nb"
`
	if err := os.WriteFile(filepath.Join(configDir, "machine.toml"), []byte(machineTOML), 0o644); err != nil {
		t.Fatalf("write machine.toml: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/machine", nil)
	w := httptest.NewRecorder()
	New(false).handleMachineStatus(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var out machineStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name != "mbp" {
		t.Errorf("Name = %q, want mbp", out.Name)
	}
	if out.ID == "" {
		t.Error("ID is empty — the handler must mint-or-read the machine identity")
	}
	if len(out.Ecosystems) != 2 {
		t.Fatalf("ecosystems = %+v, want 2", out.Ecosystems)
	}
	// Sorted by name: cloud (missing) then grovetools (present).
	if out.Ecosystems[0].Name != "cloud" || out.Ecosystems[0].State != "declared-missing" {
		t.Errorf("cloud = %+v, want declared-missing", out.Ecosystems[0])
	}
	if out.Ecosystems[1].Name != "grovetools" || out.Ecosystems[1].State != "present" {
		t.Errorf("grovetools = %+v, want present", out.Ecosystems[1])
	}
	if !out.Ecosystems[1].Enabled || out.Ecosystems[1].Notebook != "grovetools" {
		t.Errorf("grovetools lost fields: %+v", out.Ecosystems[1])
	}
	if len(out.Roots) != 1 || out.Roots[0].Name != "chickens" || !out.Roots[0].Exists {
		t.Errorf("roots = %+v, want one existing chickens root", out.Roots)
	}
}

// No machine.toml is not an error: identity still answers, with no intent.
func TestHandleMachineStatusWithoutAConfig(t *testing.T) {
	sandboxGroveHome(t)

	w := httptest.NewRecorder()
	New(false).handleMachineStatus(w, httptest.NewRequest(http.MethodGet, "/api/machine", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var out machineStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Name == "" {
		t.Error("Name is empty — it should fall back to the hostname")
	}
	if len(out.Ecosystems) != 0 || len(out.Roots) != 0 {
		t.Errorf("expected no declared intent, got %+v / %+v", out.Ecosystems, out.Roots)
	}
}

func TestHandleMachineStatusRejectsNonGET(t *testing.T) {
	sandboxGroveHome(t)
	w := httptest.NewRecorder()
	New(false).handleMachineStatus(w, httptest.NewRequest(http.MethodPost, "/api/machine", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/machine returned %d, want 405", w.Code)
	}
}
