// Memory analysis HTTP handlers — expose /api/memory/analysis/* over the
// daemon-managed memory.DocumentStore. Each handler proxies to the global
// daemon when running scoped, and combines DB aggregations with filesystem
// reality (os.Stat, IsZombieWorktree) where needed.
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
)

// memWriteJSON writes v as JSON, with the standard Content-Type header.
func memWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// memStoreReady returns true if s.memStore is initialized; otherwise it writes
// a 503 to the client.
func (s *Server) memStoreReady(w http.ResponseWriter) bool {
	if s.memStore == nil {
		http.Error(w, "memory store not initialized", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// handleMemoryAnalysisGC handles GET (dry-run) and POST (force) on /api/memory/analysis/gc.
func (s *Server) handleMemoryAnalysisGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.scope != "" {
		var (
			out *models.GCAnalysisResponse
			err error
		)
		if r.Method == http.MethodPost {
			out, err = s.forwardToGlobal().ExecuteMemoryGC(r.Context())
		} else {
			out, err = s.forwardToGlobal().GetMemoryAnalysisGC(r.Context())
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("forward gc failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}

	if !s.memStoreReady(w) {
		return
	}

	infos, err := s.memStore.GetDocumentPathInfos(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("get document path infos failed: %v", err), http.StatusInternalServerError)
		return
	}
	out := &models.GCAnalysisResponse{}
	for _, p := range infos {
		if workspace.IsZombieWorktree(p.Path) {
			out.ZombieCount++
			out.PathsToRemove = append(out.PathsToRemove, p.Path)
			continue
		}
		fi, err := os.Stat(p.Path)
		if err != nil {
			if os.IsNotExist(err) {
				out.MissingCount++
				out.PathsToRemove = append(out.PathsToRemove, p.Path)
			}
			continue
		}
		if fi.ModTime().After(p.UpdatedAt) {
			out.StaleCount++
		}
	}

	if r.Method == http.MethodPost {
		for _, p := range out.PathsToRemove {
			if err := s.memStore.DeleteDocument(r.Context(), p); err == nil {
				out.PathsRemoved = append(out.PathsRemoved, p)
			}
		}
	}

	memWriteJSON(w, out)
}

// handleMemoryAnalysisWorkspaces handles GET /api/memory/analysis/workspaces.
func (s *Server) handleMemoryAnalysisWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisWorkspaces(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward workspaces failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}

	aggs, err := s.memStore.GetNotespaceAggregations(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("workspace aggregation failed: %v", err), http.StatusInternalServerError)
		return
	}

	infos, err := s.memStore.GetDocumentPathInfos(r.Context())
	if err == nil {
		stalePerWs := map[string]int{}
		for _, p := range infos {
			if workspace.IsZombieWorktree(p.Path) {
				continue
			}
			fi, err := os.Stat(p.Path)
			if err == nil && fi.ModTime().After(p.UpdatedAt) {
				stalePerWs[p.NotespaceID]++
			}
		}
		for _, a := range aggs {
			a.StaleCount = stalePerWs[a.NotespaceID]
		}
	}

	memWriteJSON(w, aggs)
}

// handleMemoryAnalysisEcosystems handles GET /api/memory/analysis/ecosystems.
func (s *Server) handleMemoryAnalysisEcosystems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisEcosystems(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward ecosystems failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}

	infos, err := s.memStore.GetDocumentPathInfos(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("get document path infos failed: %v", err), http.StatusInternalServerError)
		return
	}
	indexed := map[string]int{}
	for _, p := range infos {
		if p.NotespaceName != "" {
			indexed[p.NotespaceName]++
		}
	}

	cfg, err := config.LoadDefault()
	if err != nil {
		http.Error(w, "loading grove config: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var out []*models.EcosystemAnalysis
	if cfg != nil {
		for name, src := range cfg.Groves {
			analysis := &models.EcosystemAnalysis{
				Name: name,
				Path: src.Path,
			}
			entries, _ := os.ReadDir(src.Path)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				analysis.ConfiguredRoots++
				wsName := e.Name()
				wsPath := filepath.Join(src.Path, wsName)
				if indexed[wsName] > 0 {
					analysis.IndexedRoots++
				} else {
					analysis.ZeroCoverage = append(analysis.ZeroCoverage, wsName)
				}
				if _, err := os.Stat(filepath.Join(wsPath, "go.mod")); os.IsNotExist(err) {
					analysis.LanguageGaps = append(analysis.LanguageGaps, wsName)
				}
			}
			out = append(out, analysis)
		}
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisCode handles GET /api/memory/analysis/code.
func (s *Server) handleMemoryAnalysisCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisCode(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward code failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetCodeAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("code analysis failed: %v", err), http.StatusInternalServerError)
		return
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisConcepts handles GET /api/memory/analysis/concepts.
func (s *Server) handleMemoryAnalysisConcepts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisConcepts(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward concepts failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetConceptAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("concept analysis failed: %v", err), http.StatusInternalServerError)
		return
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisEmbeddings handles GET /api/memory/analysis/embeddings.
func (s *Server) handleMemoryAnalysisEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisEmbeddings(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward embeddings failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetEmbeddingAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("embedding analysis failed: %v", err), http.StatusInternalServerError)
		return
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisFreshness handles GET /api/memory/analysis/freshness.
func (s *Server) handleMemoryAnalysisFreshness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisFreshness(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward freshness failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetFreshnessAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("freshness analysis failed: %v", err), http.StatusInternalServerError)
		return
	}

	if infos, err := s.memStore.GetDocumentPathInfos(r.Context()); err == nil {
		for _, p := range infos {
			if workspace.IsZombieWorktree(p.Path) {
				continue
			}
			fi, err := os.Stat(p.Path)
			if err == nil && fi.ModTime().After(p.UpdatedAt) {
				out.StaleFiles++
			}
		}
	}

	memWriteJSON(w, out)
}

// handleMemoryAnalysisDuplicates handles GET /api/memory/analysis/duplicates.
func (s *Server) handleMemoryAnalysisDuplicates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisDuplicates(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward duplicates failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetDuplicateAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("duplicate analysis failed: %v", err), http.StatusInternalServerError)
		return
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisNotebooks handles GET /api/memory/analysis/notebooks.
func (s *Server) handleMemoryAnalysisNotebooks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisNotebooks(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward notebooks failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}
	out, err := s.memStore.GetNotebookAnalysis(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("notebook analysis failed: %v", err), http.StatusInternalServerError)
		return
	}
	memWriteJSON(w, out)
}

// handleMemoryAnalysisContext handles GET /api/memory/analysis/context.
func (s *Server) handleMemoryAnalysisContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.scope != "" {
		out, err := s.forwardToGlobal().GetMemoryAnalysisContext(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("forward context failed: %v", err), http.StatusBadGateway)
			return
		}
		memWriteJSON(w, out)
		return
	}
	if !s.memStoreReady(w) {
		return
	}

	docs, err := s.memStore.GetContextPresetDocuments(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("context preset query failed: %v", err), http.StatusInternalServerError)
		return
	}

	out := &models.ContextAnalysis{TotalPresets: len(docs)}
	for _, d := range docs {
		stat := models.ContextPresetStat{NotespaceID: d.NotespaceID, NotespaceName: d.NotespaceName, Path: d.Path}
		for _, ref := range parsePresetReferences(d.Content) {
			stat.FileCount++
			if _, err := os.Stat(ref); os.IsNotExist(err) {
				stat.MissingFiles++
			}
		}
		out.Presets = append(out.Presets, stat)
	}
	memWriteJSON(w, out)
}

// parsePresetReferences extracts plausible file references from a cx rules
// file. It strips comments/blank lines and `@alias:` prefixes; remaining
// non-empty entries are treated as candidate paths. Aliased paths are not
// resolved here — they're skipped for the missing-file check.
func parsePresetReferences(content string) []string {
	var refs []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			// Aliased reference (e.g. "@core:pkg/foo.go") — skip filesystem check.
			continue
		}
		// Strip inline comment.
		if i := strings.Index(line, " #"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		refs = append(refs, line)
	}
	return refs
}
