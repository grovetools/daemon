package enrichment

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/core/util/delegation"
)

// FetchNoteCountsMap fetches note counts for all known workspaces.
// Returns counts indexed by workspace name (not path).
// The caller should map workspace names to paths as needed.
func FetchNoteCountsMap() (map[string]*models.NoteCounts, error) {
	nbPath := filepath.Join(paths.BinDir(), "nb")
	var cmd *exec.Cmd
	if _, err := os.Stat(nbPath); err == nil {
		cmd = exec.Command(nbPath, "list", "--workspaces", "--json")
	} else {
		cmd = delegation.Command("nb", "list", "--workspaces", "--json")
	}

	output, err := cmd.Output()
	if err != nil {
		return make(map[string]*models.NoteCounts), nil
	}

	type nbNote struct {
		Type      string `json:"type"`
		Workspace string `json:"workspace"`
	}

	var notes []nbNote
	if err := json.Unmarshal(output, &notes); err != nil {
		return make(map[string]*models.NoteCounts), fmt.Errorf("failed to unmarshal nb output: %w", err)
	}

	countsByName := make(map[string]*models.NoteCounts)
	for _, note := range notes {
		if _, ok := countsByName[note.Workspace]; !ok {
			countsByName[note.Workspace] = &models.NoteCounts{}
		}
		switch note.Type {
		case "current":
			countsByName[note.Workspace].Current++
		case "issues":
			countsByName[note.Workspace].Issues++
		case "inbox":
			countsByName[note.Workspace].Inbox++
		case "docs":
			countsByName[note.Workspace].Docs++
		case "completed":
			countsByName[note.Workspace].Completed++
		case "review":
			countsByName[note.Workspace].Review++
		case "in-progress":
			countsByName[note.Workspace].InProgress++
		default:
			countsByName[note.Workspace].Other++
		}
	}

	return countsByName, nil
}
