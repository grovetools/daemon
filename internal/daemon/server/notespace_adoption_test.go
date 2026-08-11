package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/notespace"
)

func TestExplicitAdoptMintsOnceBeforeNotification(t *testing.T) {
	configDir := sandboxGroveHome(t)
	scan := t.TempDir()
	repo := filepath.Join(scan, "fresh")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	notebook := t.TempDir()
	if err := os.Mkdir(filepath.Join(notebook, "notespaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAdoptionConfig(t, configDir, scan, notebook)

	req := models.NotespaceAdoption{Root: repo, Subject: "github.com/acme/Fresh", Kind: "repo", DisplayName: "fresh"}
	s := New(false)
	notified := 0
	s.SetNotespaceAdopted(func(root, name string) {
		notified++
		stamp, err := notespace.LoadNotespace(root)
		if err != nil || stamp == nil {
			t.Fatalf("notification ran before durable stamp: stamp=%+v err=%v", stamp, err)
		}
	})

	first := postAdoption(t, s, req)
	if !first.Minted || first.NotespaceID == "" || notified != 1 {
		t.Fatalf("first adoption = %+v notified=%d", first, notified)
	}
	second := postAdoption(t, s, req)
	if second.Minted || second.NotespaceID != first.NotespaceID || notified != 2 {
		t.Fatalf("idempotent adoption = %+v, first=%+v notified=%d", second, first, notified)
	}
}

func TestMissingStampWithoutRecordedScanRootNeverMints(t *testing.T) {
	configDir := sandboxGroveHome(t)
	recordedScan := t.TempDir()
	outside := t.TempDir()
	repo := filepath.Join(outside, "arbitrary")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	notebook := t.TempDir()
	if err := os.Mkdir(filepath.Join(notebook, "notespaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAdoptionConfig(t, configDir, recordedScan, notebook)

	body, _ := json.Marshal(models.NotespaceAdoption{Root: repo, Subject: "github.com/acme/arbitrary", Kind: "repo", DisplayName: "arbitrary"})
	w := httptest.NewRecorder()
	New(false).handleNotespaceAdopt(w, httptest.NewRequest(http.MethodPost, "/api/notespaces/adopt", bytes.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(notebook, "notespaces", "arbitrary", notespace.NotespaceStampName)); !os.IsNotExist(err) {
		t.Fatalf("arbitrary missing stamp was mutated: %v", err)
	}
}

func writeAdoptionConfig(t *testing.T, configDir, scan, notebook string) {
	t.Helper()
	roots := "[roots.code]\npath = " + strconvQuote(scan) + "\nscan = true\nnotebook = \"notes\"\ndepth = 2\n"
	notebooks := "default = \"notes\"\n[notebooks.notes]\nroot = " + strconvQuote(notebook) + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "roots.toml"), []byte(roots), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "notebooks.toml"), []byte(notebooks), 0o600); err != nil {
		t.Fatal(err)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func postAdoption(t *testing.T, s *Server, req models.NotespaceAdoption) models.NotespaceAdoptionResult {
	t.Helper()
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	s.handleNotespaceAdopt(w, httptest.NewRequest(http.MethodPost, "/api/notespaces/adopt", bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out models.NotespaceAdoptionResult
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
