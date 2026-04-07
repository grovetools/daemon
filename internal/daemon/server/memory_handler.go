// Memory HTTP handlers — expose the daemon-managed memory.DocumentStore via
// /api/memory/{search,coverage,status}. The store and embedder are the same
// ones used by the MemoryHandler watcher, so there is no WAL contention and
// API-key plumbing lives only in daemon cmd.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	memory "github.com/grovetools/memory/pkg/memory"
)

// SetMemoryStore wires the memory store + embedder into the server so HTTP
// handlers can serve /api/memory/* requests. Called from cmd/groved.go after
// the MemoryHandler watcher has been constructed with the same instances.
func (s *Server) SetMemoryStore(store memory.DocumentStore, embedder *memory.Embedder, dbPath string) {
	s.memStore = store
	s.memEmbedder = embedder
	s.memDBPath = dbPath
}

// handleMemorySearch handles POST /api/memory/search.
func (s *Server) handleMemorySearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memStore == nil {
		http.Error(w, "memory store not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.MemorySearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Build base search config with optional FTS/vector toggles.
	searchCfg := memory.DefaultSearchConfig()
	if req.Limit > 0 {
		searchCfg.Limit = req.Limit
	}
	searchCfg.DocTypeFilter = req.DocTypeFilter
	// Default: both on. Only override when the client explicitly selects a single mode.
	if req.UseFTS || req.UseVector {
		searchCfg.UseFTS = req.UseFTS
		searchCfg.UseVector = req.UseVector
	}

	// Apply per-doctype scoping and boosts from the grove.toml [memory] section.
	memCfg := memory.DefaultConfig()
	if coreCfg, err := config.LoadDefault(); err == nil && coreCfg != nil {
		_ = coreCfg.UnmarshalExtension("memory", &memCfg)
	}
	searchCfg = memCfg.BuildSearchConfig(searchCfg, req.Scope, req.WorkspacePath)

	// Embed the query if vector search is enabled.
	var queryEmb []float32
	if searchCfg.UseVector {
		if s.memEmbedder == nil {
			http.Error(w, "embedder not initialized", http.StatusServiceUnavailable)
			return
		}
		emb, err := s.memEmbedder.EmbedQuery(r.Context(), req.Query)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to embed query: %v", err), http.StatusInternalServerError)
			return
		}
		queryEmb = emb
	}

	results, err := s.memStore.Search(r.Context(), req.Query, queryEmb, searchCfg)
	if err != nil {
		http.Error(w, fmt.Sprintf("search failed: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]models.MemorySearchResult, len(results))
	for i, r := range results {
		out[i] = models.MemorySearchResult{
			DocumentID: r.DocumentID,
			ChunkID:    r.ChunkID,
			DocType:    r.DocType,
			Content:    r.Content,
			Path:       r.Path,
			Metadata:   r.Metadata,
			Score:      r.Score,
			FTSRank:    r.FTSRank,
			VectorRank: r.VectorRank,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleMemoryCoverage handles POST /api/memory/coverage.
func (s *Server) handleMemoryCoverage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memStore == nil {
		http.Error(w, "memory store not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.MemoryCoverageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	tolerance := req.Tolerance
	if tolerance <= 0 {
		tolerance = 1.0
	}

	report, err := s.memStore.CalculateCoverage(r.Context(), req.TargetPath, tolerance)
	if err != nil {
		http.Error(w, fmt.Sprintf("coverage failed: %v", err), http.StatusInternalServerError)
		return
	}

	out := models.MemoryCoverageReport{
		OverallCoveragePercentage: report.OverallCoveragePercentage,
		AdaptiveThreshold:         report.AdaptiveThreshold,
		Tolerance:                 report.Tolerance,
		Files:                     make([]models.MemoryFileCoverage, len(report.Files)),
	}
	for i, f := range report.Files {
		out.Files[i] = models.MemoryFileCoverage{
			Path:                   f.Path,
			TotalChunks:            f.TotalChunks,
			UncoveredChunks:        f.UncoveredChunks,
			CoveragePercentage:     f.CoveragePercentage,
			NearestConceptPath:     f.NearestConceptPath,
			NearestConceptDistance: f.NearestConceptDistance,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleMemoryStatus handles GET /api/memory/status.
func (s *Server) handleMemoryStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.memStore == nil {
		http.Error(w, "memory store not initialized", http.StatusServiceUnavailable)
		return
	}

	stats, err := s.memStore.GetStats(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("stats failed: %v", err), http.StatusInternalServerError)
		return
	}

	out := models.MemoryStatusResponse{
		DBPath:   s.memDBPath,
		DocTypes: map[string]int{},
	}
	if docs, ok := stats["documents"].(int); ok {
		out.Documents = docs
	}
	if chunks, ok := stats["chunks_vec"].(int); ok {
		out.ChunksVec = chunks
	}
	if types, ok := stats["doc_types"].(map[string]int); ok {
		out.DocTypes = types
	}
	if s.memDBPath != "" {
		if info, err := os.Stat(s.memDBPath); err == nil {
			out.DBSizeBytes = info.Size()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

