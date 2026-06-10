package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grovetools/core/pkg/models"
	memory "github.com/grovetools/memory/pkg/memory"
)

// stubDocumentStore records the arguments passed to Search. All other
// DocumentStore methods come from the embedded nil interface and panic if
// called — handleMemorySearch must only call Search.
type stubDocumentStore struct {
	memory.DocumentStore
	gotCfg memory.SearchConfig
	gotEmb []float32
}

func (s *stubDocumentStore) Search(_ context.Context, _ string, queryEmbedding []float32, config memory.SearchConfig) ([]memory.SearchResult, error) {
	s.gotCfg = config
	s.gotEmb = queryEmbedding
	return []memory.SearchResult{
		{DocumentID: "d1", ChunkID: 1, DocType: "note", Content: "hit", Path: "notes/a.md", Score: 0.5},
	}, nil
}

// TestHandleMemorySearch_NilEmbedderDegradesToFTS verifies that a vector
// search request against a daemon with no embedder (e.g. no Gemini API key)
// degrades to FTS-only instead of returning 503.
func TestHandleMemorySearch_NilEmbedderDegradesToFTS(t *testing.T) {
	s := New(false)
	stub := &stubDocumentStore{}
	s.SetMemoryStore(stub, nil, "")

	body := strings.NewReader(`{"query":"gateway timeout","use_fts":false,"use_vector":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/memory/search", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleMemorySearch(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", resp.StatusCode, w.Body.String())
	}

	if stub.gotCfg.UseVector {
		t.Error("expected UseVector to be downgraded to false with nil embedder")
	}
	if !stub.gotCfg.UseFTS {
		t.Error("expected UseFTS to be enabled when vector search is downgraded")
	}
	if len(stub.gotEmb) != 0 {
		t.Errorf("expected no query embedding, got %d dims", len(stub.gotEmb))
	}

	var results []models.MemorySearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(results) != 1 || results[0].Path != "notes/a.md" {
		t.Errorf("unexpected results: %+v", results)
	}
}

// TestHandleMemorySearch_NoStore verifies the 503 when no store is wired.
func TestHandleMemorySearch_NoStore(t *testing.T) {
	s := New(false)

	body := strings.NewReader(`{"query":"anything","use_fts":true,"use_vector":false}`)
	req := httptest.NewRequest(http.MethodPost, "/api/memory/search", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.handleMemorySearch(w, req)

	if w.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Result().StatusCode)
	}
}
