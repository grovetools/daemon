package watcher

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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

	addDir := func(dir string, node *workspace.WorkspaceNode) {
		if dir != "" {
			if _, err := os.Stat(dir); err == nil {
				newWatches[dir] = node
			}
		}
	}

	for _, ew := range workspaces {
		node := ew.WorkspaceNode
		if node == nil {
			continue
		}

		// Only watch high-value content types for memory indexing:
		// skills, concepts, issues, inbox, completed

		if dir, err := h.locator.GetSkillsDir(node); err == nil {
			addDir(dir, node)
		}
		// Note types: concepts, issues, inbox
		for _, noteType := range []string{"concepts", "issues", "inbox"} {
			if dir, err := h.locator.GetNotesDir(node, noteType); err == nil {
				addDir(dir, node)
			}
		}
		if dir, err := h.locator.GetCompletedDir(node); err == nil {
			addDir(dir, node)
		}
	}

	h.pathsMutex.Lock()
	h.watchedPaths = newWatches
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

	// Only process markdown and text files
	ext := strings.ToLower(filepath.Ext(event.Name))
	if ext != ".md" && ext != ".txt" {
		return false
	}

	// Content-type filtering is handled by ComputeWatchPaths — only
	// skills, concepts, issues, inbox, completed dirs are watched.
	h.pathsMutex.RLock()
	defer h.pathsMutex.RUnlock()

	for watchedPath := range h.watchedPaths {
		if event.Name == watchedPath || strings.HasPrefix(event.Name, watchedPath+string(filepath.Separator)) {
			return true
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
	for p := range h.watchedPaths {
		pathsToScan = append(pathsToScan, p)
	}
	h.pathsMutex.RUnlock()

	for _, dir := range pathsToScan {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			// Skip .artifacts directories entirely (generated content, large files)
			if d.IsDir() {
				if d.Name() == ".artifacts" {
					return filepath.SkipDir
				}
				return nil
			}

			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".md" && ext != ".txt" {
				return nil
			}

			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") {
				return nil
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

func (h *MemoryHandler) processJob(ctx context.Context, job IndexJob) {
	contentBytes, err := os.ReadFile(job.Path)
	if err != nil {
		if os.IsNotExist(err) {
			// File was deleted
			if err := h.memStore.DeleteDocument(ctx, job.Path); err != nil {
				h.log.WithError(err).WithField("path", job.Path).Debug("Failed to delete document from index (might not exist)")
			}
			return
		}
		h.log.WithError(err).WithField("path", job.Path).Warn("Failed to read file for memory indexing")
		return
	}

	content := string(contentBytes)
	content = memory.StripFrontmatter(content)
	if strings.TrimSpace(content) == "" {
		h.memStore.DeleteDocument(ctx, job.Path)
		return
	}

	// Heuristically determine DocType
	docType := "note"
	if strings.Contains(job.Path, "/skills/") {
		docType = "skill"
	} else if strings.Contains(job.Path, "/plans/") {
		docType = "plan"
	}

	chunks := memory.ChunkDocument(content, memory.DefaultChunkConfig())
	if len(chunks) == 0 {
		return
	}

	// Safety timeout for API-bound operations so the worker is never permanently stalled
	embedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	embeddings, err := h.embedder.EmbedDocuments(embedCtx, chunks)
	if err != nil {
		h.log.WithError(err).WithField("path", job.Path).Warn("Failed to embed document chunks")
		return
	}

	mappedChunks := make([]memory.Chunk, len(chunks))
	for i, text := range chunks {
		mappedChunks[i] = memory.Chunk{
			ChunkIndex: i,
			Content:    text,
			Embedding:  embeddings[i],
		}
	}

	doc := &memory.Document{
		ID:        uuid.New().String(),
		Path:      job.Path,
		DocType:   docType,
		Workspace: job.Workspace,
		Metadata:  nil,
		Content:   content,
		UpdatedAt: time.Now(),
	}

	if err := h.memStore.UpsertDocument(ctx, doc, mappedChunks); err != nil {
		h.log.WithError(err).WithField("path", job.Path).Warn("Failed to upsert document to memory index")
	} else {
		h.log.WithField("path", job.Path).Debug("Successfully indexed document into memory")
	}
}
