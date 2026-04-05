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
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/memory/pkg/memory"
	"github.com/sirupsen/logrus"
)

// IndexJob represents a file to be indexed into the memory store.
type IndexJob struct {
	Path      string
	Workspace string
}

// MemoryHandler implements DomainHandler for auto-indexing content into the memory store.
type MemoryHandler struct {
	store    *store.Store
	cfg      *config.Config
	locator  *workspace.NotebookLocator
	memStore memory.DocumentStore
	embedder *memory.Embedder
	log      *logrus.Entry

	watchedPaths map[string]*workspace.WorkspaceNode
	codePaths    map[string]bool // paths that contain Go source code (vs notebook content)
	pathsMutex   sync.RWMutex

	debounceMs  int
	timers      map[string]*time.Timer
	timersMutex sync.Mutex

	jobQueue chan IndexJob
}

// NewMemoryHandler creates a new MemoryHandler for auto-indexing content.
func NewMemoryHandler(st *store.Store, cfg *config.Config, memStore memory.DocumentStore, embedder *memory.Embedder, debounceMs int) *MemoryHandler {
	if debounceMs <= 0 {
		debounceMs = 5000
	}

	h := &MemoryHandler{
		store:        st,
		cfg:          cfg,
		locator:      workspace.NewNotebookLocator(cfg),
		memStore:     memStore,
		embedder:     embedder,
		log:          logging.NewLogger("groved.memory.watcher"),
		watchedPaths: make(map[string]*workspace.WorkspaceNode),
		codePaths:    make(map[string]bool),
		timers:       make(map[string]*time.Timer),
		debounceMs:   debounceMs,
		jobQueue:     make(chan IndexJob, 1000),
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

		// Code source directories: ecosystem sub-projects and standalone projects
		// that contain Go source code (identified by go.mod presence)
		if node.IsEcosystemChild() || node.Kind == workspace.KindStandaloneProject || node.Kind == workspace.KindStandaloneProjectWorktree {
			goModPath := filepath.Join(node.Path, "go.mod")
			if _, err := os.Stat(goModPath); err == nil {
				addCodeDir(node.Path, node)
			}
		}
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
	h.codePaths = newCodePaths
	h.pathsMutex.Unlock()

	paths := make([]string, 0, len(newWatches))
	for p := range newWatches {
		paths = append(paths, p)
	}
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
				// Code paths: accept .go files, skip generated and test files
				if ext == ".go" && !strings.HasSuffix(event.Name, "_test.go") {
					return true
				}
				return false
			}
			// Notebook paths: accept .md and .txt
			if ext == ".md" || ext == ".txt" {
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
	if update.Type == store.UpdateConfigReload {
		newCfg, err := config.LoadDefault()
		if err == nil {
			h.cfg = newCfg
			h.locator = workspace.NewNotebookLocator(newCfg)
		}
	}
}

func (h *MemoryHandler) OnStart(ctx context.Context) {
	// Trigger an initial background sync to catch any files modified while daemon was offline
	go h.fullSync(ctx)
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

func (h *MemoryHandler) triggerJob(path string) {
	h.timersMutex.Lock()
	defer h.timersMutex.Unlock()

	if timer, exists := h.timers[path]; exists {
		timer.Stop()
	}

	h.timers[path] = time.AfterFunc(time.Duration(h.debounceMs)*time.Millisecond, func() {
		wsName := resolveWorkspaceName(path)

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
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".artifacts" {
					return filepath.SkipDir
				}
				// For code paths, skip non-source directories
				if isCodeDir {
					if name == "vendor" || name == "node_modules" || name == "dist" || name == ".git" || name == "testdata" {
						return filepath.SkipDir
					}
				}
				return nil
			}

			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") {
				return nil
			}

			ext := strings.ToLower(filepath.Ext(path))
			if isCodeDir {
				if ext != ".go" || strings.HasSuffix(path, "_test.go") {
					return nil
				}
			} else {
				if ext != ".md" && ext != ".txt" {
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
				h.triggerJob(path)
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
			h.processJob(ctx, job)
		}
	}
}

var (
	goPackageRegex = regexp.MustCompile(`^package\s+([a-zA-Z0-9_]+)`)
	goModuleRegex  = regexp.MustCompile(`(?m)^module\s+([^\s]+)`)
)

// codeMetadata mirrors the CodeMetadata struct from memory/cmd for JSON encoding.
type codeMetadata struct {
	Repo     string   `json:"repo"`
	Package  string   `json:"package"`
	FilePath string   `json:"file_path"`
	Imports  []string `json:"imports,omitempty"`
}

func (h *MemoryHandler) processJob(ctx context.Context, job IndexJob) {
	contentBytes, err := os.ReadFile(job.Path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := h.memStore.DeleteDocument(ctx, job.Path); err != nil {
				h.log.WithError(err).WithField("path", job.Path).Debug("Failed to delete document from index (might not exist)")
			}
			return
		}
		h.log.WithError(err).WithField("path", job.Path).Warn("Failed to read file for memory indexing")
		return
	}

	isGoFile := strings.HasSuffix(job.Path, ".go")

	content := string(contentBytes)
	if !isGoFile {
		content = memory.StripFrontmatter(content)
	}
	if strings.TrimSpace(content) == "" {
		h.memStore.DeleteDocument(ctx, job.Path)
		return
	}

	// Skip generated Go files
	if isGoFile && isGeneratedGoFile(content) {
		return
	}

	// Determine DocType and metadata
	var docType string
	var metadataBytes []byte

	if isGoFile {
		docType = "code"
		pkgName := extractGoPackage(content)
		modName, modRoot := findGoModule(filepath.Dir(job.Path))
		relPath, err := filepath.Rel(modRoot, job.Path)
		if err != nil {
			relPath = job.Path
		}
		imports := extractGoImports(content)
		meta := codeMetadata{
			Repo:     modName,
			Package:  pkgName,
			FilePath: relPath,
			Imports:  imports,
		}
		metadataBytes, _ = json.Marshal(meta)
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
		h.log.WithError(err).WithField("path", job.Path).Debug("Failed to check existing embeddings, will embed all")
		existingEmbeddings = make(map[string][]float32)
	}

	// Only embed chunks that don't already exist
	var textsToEmbed []string
	for i, hash := range hashes {
		if _, exists := existingEmbeddings[hash]; !exists {
			textsToEmbed = append(textsToEmbed, chunks[i])
		}
	}

	var newEmbeddings [][]float32
	if len(textsToEmbed) > 0 {
		embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		newEmbeddings, err = h.embedder.EmbedDocuments(embedCtx, textsToEmbed)
		if err != nil {
			h.log.WithError(err).WithField("path", job.Path).Warn("Failed to embed document chunks")
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
		} else {
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
		ID:        uuid.New().String(),
		Path:      job.Path,
		DocType:   docType,
		Workspace: job.Workspace,
		Metadata:  metadataBytes,
		Content:   content,
		UpdatedAt: time.Now(),
	}

	if err := h.memStore.UpsertDocument(ctx, doc, mappedChunks); err != nil {
		h.log.WithError(err).WithField("path", job.Path).Warn("Failed to upsert document to memory index")
	} else {
		h.log.WithField("path", job.Path).WithField("type", docType).
			WithField("new_embeddings", len(textsToEmbed)).
			WithField("reused_embeddings", len(chunks)-len(textsToEmbed)).
			Debug("Successfully indexed document into memory")
	}
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
		if b, err := os.ReadFile(modPath); err == nil {
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
