package enrichment

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	frontmatter "github.com/grovetools/core/util/frontmatter"
)

// CountNotesInProcess counts markdown files in note directories without shelling out.
// Returns counts indexed by workspace name.
func CountNotesInProcess(nodes []*workspace.WorkspaceNode, locator *workspace.NotebookLocator) map[string]*models.NoteCounts {
	countsByName := make(map[string]*models.NoteCounts)

	for _, node := range nodes {
		dirs, err := locator.GetAllContentDirs(node)
		if err != nil {
			continue
		}

		counts := &models.NoteCounts{}
		hasCounts := false

		for _, dir := range dirs {
			if dir.Type != "notes" {
				continue
			}

			entries, err := os.ReadDir(dir.Path)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if entry.IsDir() {
					// Ignore hidden dirs like .archive
					if strings.HasPrefix(entry.Name(), ".") {
						continue
					}
					subDirPath := filepath.Join(dir.Path, entry.Name())
					count := countMarkdownFiles(subDirPath)
					if count > 0 {
						hasCounts = true
						switch entry.Name() {
						case "current":
							counts.Current += count
						case "issues":
							counts.Issues += count
						case "inbox":
							counts.Inbox += count
						case "docs":
							counts.Docs += count
						case "completed":
							counts.Completed += count
						case "review":
							counts.Review += count
						case "in-progress", "in_progress":
							counts.InProgress += count
						default:
							counts.Other += count
						}
					}
				} else if strings.HasSuffix(entry.Name(), ".md") {
					// Files directly in the root of notes dir
					counts.Other++
					hasCounts = true
				}
			}
		}

		if hasCounts {
			countsByName[node.Name] = counts
		}
	}

	return countsByName
}

// IndexNotesInProcess builds a complete note index by walking all content directories.
// Returns a map of file path -> NoteIndexEntry.
func IndexNotesInProcess(nodes []*workspace.WorkspaceNode, locator *workspace.NotebookLocator) map[string]*models.NoteIndexEntry {
	index := make(map[string]*models.NoteIndexEntry)

	// Sort nodes so ecosystem roots come first. This ensures that when
	// multiple nodes share the same content dir (ecosystem worktrees share
	// the root's notebook), the root's workspace name is used for entries.
	sorted := make([]*workspace.WorkspaceNode, len(nodes))
	copy(sorted, nodes)
	sort.SliceStable(sorted, func(i, j int) bool {
		iRoot := sorted[i].Kind == workspace.KindEcosystemRoot || sorted[i].Kind == workspace.KindStandaloneProject
		jRoot := sorted[j].Kind == workspace.KindEcosystemRoot || sorted[j].Kind == workspace.KindStandaloneProject
		if iRoot != jRoot {
			return iRoot
		}
		iSub := sorted[i].Kind == workspace.KindEcosystemSubProject
		jSub := sorted[j].Kind == workspace.KindEcosystemSubProject
		return iSub && !jSub
	})

	// Track which content dirs we've already indexed to avoid
	// ecosystem worktrees overwriting the root's entries.
	indexedDirs := make(map[string]bool)

	for _, node := range sorted {
		dirs, err := locator.GetAllContentDirs(node)
		if err != nil {
			continue
		}

		for _, dir := range dirs {
			if indexedDirs[dir.Path] {
				continue // Already indexed by another node (e.g., ecosystem root)
			}
			indexedDirs[dir.Path] = true

			entries, err := os.ReadDir(dir.Path)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if entry.IsDir() {
					if strings.HasPrefix(entry.Name(), ".") {
						continue
					}
					subDirPath := filepath.Join(dir.Path, entry.Name())
					indexDirFiles(index, subDirPath, node.Name, dir.Path, dir.Type, entry.Name())
				} else {
					ie := indexFileEntry(filepath.Join(dir.Path, entry.Name()), node.Name, dir.Path, dir.Type)
					if ie != nil {
						index[ie.Path] = ie
					}
				}
			}
		}
	}

	return index
}

// indexDirFiles indexes all files in a subdirectory of a content dir.
func indexDirFiles(index map[string]*models.NoteIndexEntry, dirPath, wsName, contentDirPath, contentDirType, subDir string) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			// Recurse into nested subdirectories (e.g., plans/my-plan/jobs/)
			if !strings.HasPrefix(entry.Name(), ".") {
				nestedSubDir := subDir + "/" + entry.Name()
				indexDirFiles(index, filepath.Join(dirPath, entry.Name()), wsName, contentDirPath, contentDirType, nestedSubDir)
			}
			continue
		}

		filePath := filepath.Join(dirPath, entry.Name())
		ie := indexFileEntry(filePath, wsName, contentDirPath, contentDirType)
		if ie != nil {
			index[ie.Path] = ie
		}
	}
}

// indexFileEntry creates a NoteIndexEntry for a single file.
func indexFileEntry(filePath, wsName, contentDirPath, contentDirType string) *models.NoteIndexEntry {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil
	}

	name := info.Name()
	entry := &models.NoteIndexEntry{
		Path:       filePath,
		Name:       name,
		ModTime:    info.ModTime(),
		Workspace:  wsName,
		ContentDir: contentDirType,
	}

	// Compute group from relative path
	relPath, _ := filepath.Rel(contentDirPath, filePath)
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	switch contentDirType {
	case "plans":
		if len(parts) > 1 {
			entry.Group = "plans/" + strings.Join(parts[:len(parts)-1], "/")
		} else {
			entry.Group = "plans"
		}
	case "chats":
		if len(parts) > 1 {
			entry.Group = "chats/" + strings.Join(parts[:len(parts)-1], "/")
		} else {
			entry.Group = "chats"
		}
	case "notes":
		if len(parts) > 1 {
			entry.Group = strings.Join(parts[:len(parts)-1], "/")
		} else {
			entry.Group = "quick"
		}
	}

	if strings.HasSuffix(name, ".md") {
		entry.Type = "note"
		// Parse frontmatter for metadata
		f, err := os.Open(filePath)
		if err == nil {
			meta, err := frontmatter.Parse(f)
			f.Close()
			if err == nil {
				entry.Title = meta.Title
				entry.ID = meta.ID
				entry.Tags = meta.Tags
				entry.PlanRef = meta.PlanRef
				if !meta.Created.IsZero() {
					entry.Created = meta.Created
				}
				if !meta.Modified.IsZero() {
					entry.ModTime = meta.Modified
				}
			}
		}
		if entry.Title == "" {
			entry.Title = strings.TrimSuffix(name, ".md")
		}
	} else if strings.Contains(filePath, ".artifacts") {
		entry.Type = "artifact"
		entry.Title = strings.TrimSuffix(name, filepath.Ext(name))
	} else {
		entry.Type = "generic"
		entry.Title = name
	}

	return entry
}

// IndexSingleNote creates a NoteIndexEntry for a single file path.
// Used for incremental indexing on NoteEvent.
func IndexSingleNote(path, wsName, contentDirPath, contentDirType string) (*models.NoteIndexEntry, error) {
	entry := indexFileEntry(path, wsName, contentDirPath, contentDirType)
	if entry == nil {
		return nil, os.ErrNotExist
	}
	return entry, nil
}

// resolveContentDirForPath finds the content directory that contains the given path.
// Returns the content dir path and type, or empty strings if not found.
func ResolveContentDirForPath(notePath string, node *workspace.WorkspaceNode, locator *workspace.NotebookLocator) (string, string) {
	dirs, err := locator.GetAllContentDirs(node)
	if err != nil {
		return "", ""
	}
	for _, dir := range dirs {
		if strings.HasPrefix(notePath, dir.Path+"/") || strings.HasPrefix(notePath, dir.Path+string(os.PathSeparator)) {
			return dir.Path, dir.Type
		}
	}
	return "", ""
}

// DeriveCountsFromIndex computes NoteCounts per workspace from an existing note index.
// This eliminates the need for a separate filesystem walk just to count notes.
func DeriveCountsFromIndex(index map[string]*models.NoteIndexEntry) map[string]*models.NoteCounts {
	countsByName := make(map[string]*models.NoteCounts)

	for _, entry := range index {
		if entry.ContentDir != "notes" {
			continue
		}

		counts, ok := countsByName[entry.Workspace]
		if !ok {
			counts = &models.NoteCounts{}
			countsByName[entry.Workspace] = counts
		}

		// Group is the subdirectory name for notes (inbox, issues, current, etc.)
		// For nested paths like "inbox/sub", use first segment
		group := entry.Group
		if idx := strings.IndexByte(group, '/'); idx >= 0 {
			group = group[:idx]
		}

		switch group {
		case "current":
			counts.Current++
		case "issues":
			counts.Issues++
		case "inbox":
			counts.Inbox++
		case "docs":
			counts.Docs++
		case "completed":
			counts.Completed++
		case "review":
			counts.Review++
		case "in-progress", "in_progress":
			counts.InProgress++
		default:
			counts.Other++
		}
	}

	return countsByName
}

func countMarkdownFiles(dirPath string) int {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			count++
		}
	}
	return count
}
