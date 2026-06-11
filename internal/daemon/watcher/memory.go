package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/memory/pkg/memory"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

// IndexJob represents a file to be indexed into the memory store.
type IndexJob struct {
	Path      string
	Workspace string
	// Bulk marks the job as part of a bulk ingest (e.g. a Phase 1 sync
	// snapshot pull). Bulk jobs are rate-limited by the high-capacity bulk
	// limiter instead of the 2/sec steady-state limiter, so ingesting a
	// large workspace doesn't backlog embedding for hours.
	Bulk bool
}

// MemoryHandler implements DomainHandler for auto-indexing content into the memory store.
type MemoryHandler struct {
	store    *store.Store
	cfg      *config.Config
	locator  *workspace.NotebookLocator
	memStore memory.DocumentStore
	embedder memory.Embedder
	ulog     *logging.UnifiedLogger

	watchedPaths map[string]*workspace.WorkspaceNode
	codePaths    map[string]bool // paths that contain source code (vs notebook content)
	pathsMutex   sync.RWMutex

	debounceMs  int
	timers      map[string]*time.Timer
	timersMutex sync.Mutex

	jobQueue    chan IndexJob
	limiter     *rate.Limiter
	bulkLimiter *rate.Limiter
	initialSync sync.Once

	jobsQueued   atomic.Int32
	jobsInFlight atomic.Int32
}

// NewMemoryHandler creates a new MemoryHandler for auto-indexing content.
func NewMemoryHandler(st *store.Store, cfg *config.Config, memStore memory.DocumentStore, embedder memory.Embedder, debounceMs int) *MemoryHandler {
	if debounceMs <= 0 {
		debounceMs = 5000
	}

	h := &MemoryHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		memStore:     memStore,
		embedder:     embedder,
		ulog:         logging.NewUnifiedLogger("groved.watcher.memory"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		codePaths:    make(map[string]bool),
		timers:       make(map[string]*time.Timer),
		debounceMs:   debounceMs,
		jobQueue:     make(chan IndexJob, 1000),
		limiter:      rate.NewLimiter(rate.Limit(2), 1), // 2 embeddings/sec steady-state
		// Bulk-ingest limiter: an order of magnitude above steady-state so
		// snapshot pulls finish in minutes, but still bounded so a bulk
		// ingest can't hammer the embedding API without limit.
		bulkLimiter: rate.NewLimiter(rate.Limit(20), 4),
	}

	// Start a worker pool (2 workers) to handle embedding generation asynchronously
	for i := 0; i < 2; i++ {
		go h.worker(context.Background())
	}

	return h
}

func (h *MemoryHandler) Name() string {
	return "memory"
}

func (h *MemoryHandler) ComputeWatchPaths(workspaces []*models.EnrichedWorkspace) []string {
	newWatches := make(map[string]*workspace.WorkspaceNode)
	newCodePaths := make(map[string]bool)

	addDir := func(dir string, node *workspace.WorkspaceNode) {
		if dir != "" {
			if _, err := os.Stat(dir); err == nil {
				newWatches[dir] = node
			}
		}
	}

	addCodeDir := func(dir string, node *workspace.WorkspaceNode) {
		if dir != "" {
			if _, err := os.Stat(dir); err == nil {
				newWatches[dir] = node
				newCodePaths[dir] = true
			}
		}
	}

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		// Notebook content: skills, concepts, issues, inbox, completed
		if dir, err := h.locator.GetSkillsDir(node); err == nil {
			addDir(dir, node)
		}
		for _, noteType := range []string{"concepts", "issues", "inbox"} {
			if dir, err := h.locator.GetNotesDir(node, noteType); err == nil {
				addDir(dir, node)
			}
		}
		if dir, err := h.locator.GetCompletedDir(node); err == nil {
			addDir(dir, node)
		}
		if dir, err := h.locator.GetContextPresetsDir(node); err == nil {
			addDir(dir, node)
		}

		// Code source directories: ecosystem sub-projects and standalone projects
		// that contain source code (detected via language-specific project markers)
		if node.IsEcosystemChild() || node.Kind == workspace.KindStandaloneProject || node.Kind == workspace.KindStandaloneProjectWorktree {
			for _, marker := range allProjectMarkers() {
				markerPath := filepath.Join(node.Path, marker)
				if _, err := os.Stat(markerPath); err == nil {
					addCodeDir(node.Path, node)
					break
				}
			}
		}
	}

	h.pathsMutex.RLock()
	var removedPrefixes []string
	for oldPath, oldNode := range h.watchedPaths {
		if _, exists := newWatches[oldPath]; !exists {
			if oldNode != nil && oldNode.IsProjectWorktreeChild() {
				removedPrefixes = append(removedPrefixes, oldPath)
			}
		}
	}
	h.pathsMutex.RUnlock()

	if len(removedPrefixes) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for _, prefix := range removedPrefixes {
				cleanPrefix := strings.TrimRight(prefix, string(filepath.Separator)) + string(filepath.Separator)
				if err := h.memStore.DeleteDocumentsByPrefix(ctx, cleanPrefix); err != nil {
					h.ulog.Warn("Failed to GC memory for removed worktree").Err(err).Field("prefix", cleanPrefix).Log(ctx)
				} else {
					h.ulog.Info("GCed memory for removed worktree").Field("prefix", cleanPrefix).Log(ctx)
					_ = h.memStore.LogAudit(ctx, "gc_worktree", cleanPrefix, nil)
				}
			}
		}()
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.codePaths = newCodePaths
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}

	h.initialSync.Do(func() {
		go h.fullSync(context.Background())
	})

	return paths
}

func (h *MemoryHandler) MatchesEvent(event fsnotify.Event) bool {
	if event.Op&fsnotify.Chmod == fsnotify.Chmod {
		return false
	}

	// Skip hidden files/directories (but allow .archive which contains history)
	baseName := filepath.Base(event.Name)
	if strings.HasPrefix(baseName, ".") && baseName != ".archive" {
		return false
	}

	// Skip .artifacts directories (generated briefings, aggregated contexts)
	if strings.Contains(event.Name, "/.artifacts/") {
		return false
	}

	ext := strings.ToLower(filepath.Ext(event.Name))

	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if event.Name == watchedPath || strings.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) {
			if h.codePaths[watchedPath] {
				profile := profileForExt(ext)
				if profile == nil {
					return false
				}
				if isTestFile(event.Name, profile) {
					return false
				}
				return true
			}
			// Notebook paths: accept .md, .txt, .rules (cx context presets), and .yml/.yaml (concept manifests)
			if ext == ".md" || ext == ".txt" || ext == ".rules" || ext == ".yml" || ext == ".yaml" {
				return true
			}
			return false
		}
	}
	return false
}

func (h *MemoryHandler) HandleEvents(ctx context.Context, events []fsnotify.Event) error {
	for _, event := range events {
		h.triggerJob(event.Name)
	}
	return nil
}

func (h *MemoryHandler) HandleStoreUpdate(update store.Update) {
	switch update.Type {
	case store.UpdateConfigReload:
		newCfg, err := config.LoadDefault()
		if err == nil {
			h.cfg = newCfg
			h.locator = workspace.NewNotebookLocator(newCfg)
		}
	case store.UpdateMemoryReindex:
		payload, ok := update.Payload.(*store.MemoryReindexPayload)
		if !ok || payload == nil {
			return
		}
		go h.handleReindex(payload)
	}
}

func (h *MemoryHandler) handleReindex(payload *store.MemoryReindexPayload) {
	ctx := context.Background()
	switch payload.Mode {
	case "path":
		h.queueDirect(payload.Target)
	case "all":
		infos, err := h.memStore.GetDocumentPathInfos(ctx)
		if err != nil {
			h.ulog.Warn("Reindex: failed to query documents").Err(err).Log(ctx)
			return
		}
		for _, info := range infos {
			h.queueDirect(info.Path)
		}
	case "stale":
		infos, err := h.memStore.GetDocumentPathInfos(ctx)
		if err != nil {
			h.ulog.Warn("Reindex: failed to query documents").Err(err).Log(ctx)
			return
		}
		for _, info := range infos {
			fi, err := os.Stat(info.Path)
			if err != nil {
				continue
			}
			if fi.ModTime().After(info.UpdatedAt) {
				h.queueDirect(info.Path)
			}
		}
	}
}

func (h *MemoryHandler) OnStart(ctx context.Context) {
	// Startup reconciliation is deferred to ComputeWatchPaths via initialSync
	// to avoid a race where fullSync runs before watch paths are populated.
}

// resolveWorkspaceName extracts the workspace name from a file path by finding
// the "workspaces/<name>/" segment in the path. This is robust for centralized
// notebook layouts where paths follow: <root>/workspaces/<workspace-name>/...
func resolveWorkspaceName(filePath string) string {
	const marker = "/workspaces/"
	idx := strings.LastIndex(filePath, marker)
	if idx < 0 {
		return ""
	}
	rest := filePath[idx+len(marker):]
	if slash := strings.IndexByte(rest, '/'); slash > 0 {
		return rest[:slash]
	}
	return rest
}

// queueDirect pushes a job directly onto the jobQueue without debounce timers.
// Used by fullSync and reindex to avoid creating thousands of timers.
// Sends on a goroutine so large reindex operations don't drop jobs.
func (h *MemoryHandler) queueDirect(path string) {
	wsName := resolveWorkspaceName(path)
	h.jobsQueued.Add(1)
	go func() {
		h.jobQueue <- IndexJob{Path: path, Workspace: wsName}
	}()
}

// QueueBulkIndex enqueues a file for indexing in bulk-ingest mode: the job
// is rate-limited by the high-capacity bulk limiter instead of the 2/sec
// steady-state limiter. Used for high-throughput ingestion such as sync
// snapshot pulls; not used by the steady-state fsnotify path.
func (h *MemoryHandler) QueueBulkIndex(path string) {
	wsName := resolveWorkspaceName(path)
	h.jobsQueued.Add(1)
	go func() {
		h.jobQueue <- IndexJob{Path: path, Workspace: wsName, Bulk: true}
	}()
}

func (h *MemoryHandler) triggerJob(path string) {
	h.timersMutex.Lock()
	defer h.timersMutex.Unlock()

	if timer, exists := h.timers[path]; exists {
		timer.Stop()
	}

	h.timers[path] = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		wsName := resolveWorkspaceName(path)
		h.jobsQueued.Add(1)
		h.jobQueue <- IndexJob{
			Path:      path,
			Workspace: wsName,
		}
	})
}

func (h *MemoryHandler) fullSync(ctx context.Context) {
	h.pathsMutex.RLock()
	pathsToScan := make([]string, 0, len(h.watchedPaths))
	codePathsCopy := make(map[string]bool, len(h.codePaths))
	for p := range h.watchedPaths {
		pathsToScan = append(pathsToScan, p)
	}
	for p := range h.codePaths {
		codePathsCopy[p] = true
	}
	h.pathsMutex.RUnlock()

	for _, dir := range pathsToScan {
		isCodeDir := codePathsCopy[dir]
		excludeDirs := allExcludeDirs()
		supportedExts := allSupportedExtensions()
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".artifacts" {
					return filepath.SkipDir
				}
				if isCodeDir && excludeDirs[name] {
					return filepath.SkipDir
				}
				return nil
			}

			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") {
				return nil
			}

			ext := strings.ToLower(filepath.Ext(path))
			if isCodeDir {
				profile := profileForExt(ext)
				if profile == nil || isTestFile(path, profile) {
					return nil
				}
				if !supportedExts[ext] {
					return nil
				}
			} else {
				if ext != ".md" && ext != ".txt" && ext != ".rules" && ext != ".yml" && ext != ".yaml" {
					return nil
				}
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}

			// Upsert if the file isn't in the DB or is newer than the DB timestamp
			doc, err := h.memStore.GetDocumentByPath(ctx, path)
			if err != nil || doc == nil || info.ModTime().After(doc.UpdatedAt) {
				h.queueDirect(path)
			}

			return nil
		})
	}
}

func (h *MemoryHandler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-h.jobQueue:
			h.jobsQueued.Add(-1)
			h.jobsInFlight.Add(1)
			h.pushLiveProgress()
			h.processJob(ctx, job)
			h.jobsInFlight.Add(-1)
			h.pushLiveProgress()
		}
	}
}

func (h *MemoryHandler) pushLiveProgress() {
	h.memStore.SetLiveProgress(int(h.jobsQueued.Load()), int(h.jobsInFlight.Load()))
}

var (
	goPackageRegex = regexp.MustCompile(`^package\s+([a-zA-Z0-9_]+)`)
	goModuleRegex  = regexp.MustCompile(`(?m)^module\s+([^\s]+)`)
)

// conceptManifest represents the YAML structure of concept-manifest.yml files.
type conceptManifest struct {
	ID              string   `yaml:"id"              json:"id"`
	Title           string   `yaml:"title"           json:"title"`
	Description     string   `yaml:"description"     json:"description"`
	Status          string   `yaml:"status"          json:"status"`
	RelatedConcepts []string `yaml:"related_concepts" json:"related_concepts,omitempty"`
	RelatedPlans    []string `yaml:"related_plans"    json:"related_plans,omitempty"`
	RelatedNotes    []string `yaml:"related_notes"    json:"related_notes,omitempty"`
	RelatedSkills   []string `yaml:"related_skills"   json:"related_skills,omitempty"`
}

// codeMetadata mirrors the CodeMetadata struct from memory/cmd for JSON encoding.
type codeMetadata struct {
	Repo          string   `json:"repo"`
	Package       string   `json:"package"`
	FilePath      string   `json:"file_path"`
	Language      string   `json:"language"`
	Imports       []string `json:"imports,omitempty"`
	CanonicalPath string   `json:"canonical_path,omitempty"`
	IsWorktree    bool     `json:"is_worktree,omitempty"`
}

func (h *MemoryHandler) processJob(ctx context.Context, job IndexJob) {
	if workspace.IsZombieWorktree(job.Path) {
		_ = h.memStore.DeleteDocument(ctx, job.Path)
		_ = h.memStore.LogAudit(ctx, "delete", job.Path, map[string]any{"reason": "zombie_worktree"})
		h.broadcastMemoryEvent("delete", job.Path)
		return
	}

	contentBytes, err := os.ReadFile(job.Path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := h.memStore.DeleteDocument(ctx, job.Path); err != nil {
				h.ulog.Debug("Failed to delete document from index (might not exist)").
					Err(err).
					Field("path", job.Path).
					Log(ctx)
			} else {
				_ = h.memStore.LogAudit(ctx, "delete", job.Path, map[string]any{"reason": "file_removed"})
				h.broadcastMemoryEvent("delete", job.Path)
			}
			return
		}
		h.ulog.Warn("Failed to read file for memory indexing").
			Err(err).
			Field("path", job.Path).
			Log(ctx)
		return
	}

	h.pathsMutex.RLock()
	var bestNode *workspace.WorkspaceNode
	var bestLen int
	for watchPath, node := range h.watchedPaths {
		if strings.HasPrefix(job.Path, watchPath) && len(watchPath) > bestLen {
			bestLen = len(watchPath)
			bestNode = node
		}
	}
	h.pathsMutex.RUnlock()

	var isWorktreeOverride bool
	var canonicalFilePath string

	if bestNode != nil && bestNode.IsProjectWorktreeChild() {
		relPath, err := filepath.Rel(bestNode.Path, job.Path)
		if err == nil {
			canonicalFilePath = filepath.Join(bestNode.ParentProjectPath, relPath)
			canonicalBytes, err := os.ReadFile(canonicalFilePath) //nolint:gosec // G304: path from indexed project
			if err == nil && string(canonicalBytes) == string(contentBytes) {
				_ = h.memStore.DeleteDocument(ctx, job.Path)
				_ = h.memStore.LogAudit(ctx, "delete", job.Path, map[string]any{"reason": "worktree_duplicate"})
				h.broadcastMemoryEvent("delete", job.Path)
				return
			}
			isWorktreeOverride = true
		}
	}

	ext := strings.ToLower(filepath.Ext(job.Path))
	profile := profileForExt(ext)
	isCodeFile := profile != nil

	content := string(contentBytes)
	if !isCodeFile {
		content = memory.StripFrontmatter(content)
	}
	if strings.TrimSpace(content) == "" {
		_ = h.memStore.DeleteDocument(ctx, job.Path)
		return
	}

	// Skip generated Go files
	if profile != nil && profile.Name == "go" && isGeneratedGoFile(content) {
		return
	}

	// Determine DocType and metadata
	var docType string
	var metadataBytes []byte

	if isCodeFile {
		docType = "code"
		repo, pkg, imports := profile.ExtractMeta(content, job.Path)
		// For Go, findGoModule returns modRoot needed for relPath
		filePath := job.Path
		if profile.Name == "go" {
			_, modRoot := findGoModule(filepath.Dir(job.Path))
			if rel, err := filepath.Rel(modRoot, job.Path); err == nil {
				filePath = rel
			}
		} else {
			filePath = filepath.Base(job.Path)
		}
		meta := codeMetadata{
			Repo:          repo,
			Package:       pkg,
			FilePath:      filePath,
			Language:      profile.Name,
			Imports:       imports,
			CanonicalPath: canonicalFilePath,
			IsWorktree:    isWorktreeOverride,
		}
		metadataBytes, _ = json.Marshal(meta)
	} else if filepath.Base(job.Path) == "concept-manifest.yml" && strings.Contains(job.Path, "/concepts/") {
		docType = "concept_manifest"
		var manifest conceptManifest
		if err := yaml.Unmarshal(contentBytes, &manifest); err == nil {
			metadataBytes, _ = json.Marshal(manifest)
		}
	} else {
		docType = "note"
		if strings.Contains(job.Path, "/skills/") {
			docType = "skill"
		} else if strings.Contains(job.Path, "/concepts/") {
			docType = "concept"
		} else if strings.Contains(job.Path, "/plans/") {
			docType = "plan"
		} else if strings.Contains(job.Path, "/issues/") {
			docType = "issue"
		}
	}

	chunks := memory.ChunkDocument(content, memory.DefaultChunkConfig())
	if len(chunks) == 0 {
		return
	}

	// Compute content hashes for deduplication
	hashes := make([]string, len(chunks))
	for i, text := range chunks {
		hash := sha256.Sum256([]byte(text))
		hashes[i] = hex.EncodeToString(hash[:])
	}

	// Check which chunks already have embeddings (content-hash dedup)
	existingEmbeddings, err := h.memStore.GetExistingEmbeddingsByHash(ctx, hashes)
	if err != nil {
		h.ulog.Debug("Failed to check existing embeddings, will embed all").
			Err(err).
			Field("path", job.Path).
			Log(ctx)
		existingEmbeddings = make(map[string][]float32)
	}

	// Only embed chunks that don't already exist
	var textsToEmbed []string
	for i, hash := range hashes {
		if _, exists := existingEmbeddings[hash]; !exists {
			textsToEmbed = append(textsToEmbed, chunks[i])
		}
	}

	// Without an embedder (e.g. no Gemini API key), index chunks with empty
	// embeddings: they remain FTS-searchable and vectors can be backfilled
	// once an embedder is configured.
	var newEmbeddings [][]float32
	if len(textsToEmbed) > 0 && h.embedder != nil {
		// Bulk-ingest jobs (sync snapshot pulls) bypass the 2/sec
		// steady-state limiter and go through the high-capacity bulk
		// limiter instead.
		limiter := h.limiter
		if job.Bulk {
			limiter = h.bulkLimiter
		}
		if err := limiter.Wait(ctx); err != nil {
			h.ulog.Debug("Rate limiter cancelled").Err(err).Field("path", job.Path).Log(ctx)
			return
		}
		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		newEmbeddings, err = h.embedder.EmbedDocuments(embedCtx, textsToEmbed)
		if err != nil {
			h.ulog.Warn("Failed to embed document chunks").
				Err(err).
				Field("path", job.Path).
				Field("chunks_to_embed", len(textsToEmbed)).
				Log(ctx)
			_ = h.memStore.LogAudit(ctx, "embed_error", job.Path, map[string]any{
				"error":  err.Error(),
				"chunks": len(textsToEmbed),
			})
			return
		}
	}

	// Reconstruct chunks with embeddings (reusing existing where possible)
	mappedChunks := make([]memory.Chunk, len(chunks))
	newEmbIdx := 0
	for i, text := range chunks {
		hash := hashes[i]
		var emb []float32
		if existing, ok := existingEmbeddings[hash]; ok {
			emb = existing
		} else if newEmbIdx < len(newEmbeddings) {
			emb = newEmbeddings[newEmbIdx]
			newEmbIdx++
		}
		mappedChunks[i] = memory.Chunk{
			ChunkIndex:  i,
			Content:     text,
			ContentHash: hash,
			Embedding:   emb,
		}
	}

	doc := &memory.Document{
		// ID left empty: the store keeps the existing stable id for this
		// path (or derives one), instead of churning identity per upsert.
		Path:      job.Path,
		DocType:   docType,
		Workspace: job.Workspace,
		Metadata:  metadataBytes,
		Content:   content,
		UpdatedAt: time.Now(),
	}

	if err := h.memStore.UpsertDocument(ctx, doc, mappedChunks); err != nil {
		h.ulog.Warn("Failed to upsert document to memory index").
			Err(err).
			Field("path", job.Path).
			Log(ctx)
	} else {
		_ = h.memStore.LogAudit(ctx, "upsert", job.Path, map[string]any{
			"doc_type":       docType,
			"chunks":         len(chunks),
			"new_embeddings": len(newEmbeddings),
			"reused":         len(chunks) - len(textsToEmbed),
		})
		h.broadcastMemoryEvent("upsert", job.Path)
	}
}

// broadcastMemoryEvent publishes a store.UpdateMemoryIndex event so SSE
// subscribers (the memory TUI) can render a transient syncing indicator.
// The event is fire-and-forget; failures are logged but never block indexing.
func (h *MemoryHandler) broadcastMemoryEvent(op, path string) {
	if h.store == nil {
		return
	}
	h.store.ApplyUpdate(store.Update{
		Type:   store.UpdateMemoryIndex,
		Source: "memory",
		Payload: &store.MemoryIndexPayload{
			Op:   op,
			Path: path,
		},
	})
}

// isGeneratedGoFile checks the first few lines for a "Code generated" comment.
func isGeneratedGoFile(content string) bool {
	lines := strings.SplitN(content, "\n", 10)
	for _, line := range lines {
		if strings.Contains(line, "Code generated") {
			return true
		}
	}
	return false
}

// extractGoPackage extracts the package name from Go source content.
func extractGoPackage(content string) string {
	lines := strings.SplitN(content, "\n", 50)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if matches := goPackageRegex.FindStringSubmatch(line); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// extractGoImports extracts import paths from Go source content.
func extractGoImports(content string) []string {
	var imports []string
	inBlock := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "import (" {
			inBlock = true
			continue
		}
		if inBlock {
			if line == ")" {
				break
			}
			// Extract the import path (strip alias and quotes)
			parts := strings.Fields(line)
			for _, p := range parts {
				p = strings.Trim(p, `"`)
				if strings.Contains(p, "/") || strings.Contains(p, ".") {
					imports = append(imports, p)
					break
				}
			}
			continue
		}
		if strings.HasPrefix(line, "import ") && strings.Contains(line, `"`) {
			// Single import: import "path"
			idx := strings.Index(line, `"`)
			if idx >= 0 {
				rest := line[idx+1:]
				if end := strings.Index(rest, `"`); end >= 0 {
					imports = append(imports, rest[:end])
				}
			}
		}
	}
	return imports
}

// findGoModule walks up from startDir to find a go.mod file and returns
// the module name and root directory.
func findGoModule(startDir string) (string, string) {
	current := startDir
	for {
		modPath := filepath.Join(current, "go.mod")
		if b, err := os.ReadFile(modPath); err == nil { //nolint:gosec // G304: go.mod from project tree
			matches := goModuleRegex.FindStringSubmatch(string(b))
			name := filepath.Base(current)
			if len(matches) > 1 {
				name = strings.TrimSpace(matches[1])
			}
			return name, current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return filepath.Base(startDir), startDir
}
