package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// The /api/sync/* payload structs in this package are private, and core mirrors
// them by hand in pkg/models for every client that reads them. Nothing tied the
// two together, so the notespace-identity rework renamed the daemon's keys
// (workspaces → notespaces, name → notespace_id/notespace_name, hydration's
// workspace → notespace) and core's mirrors kept decoding the old ones: every
// per-notespace row silently arrived empty, `grove sync adopt` reported "the
// daemon is no longer tracking this workspace" for workspaces it was tracking
// fine, and the client's own test kept passing because its fixture was
// hand-written to the mirror rather than to the daemon.
//
// These tests are that missing tie. They are deliberately STRICT: a field added
// on either side fails until both sides carry it, which is the coupling these
// payloads already had and were not honest about.

// assertMirrors round-trips a daemon payload through its core mirror and fails
// unless the JSON comes back byte-equivalent as a decoded object. A renamed or
// missing key on the core side drops out of the re-encoded form; a key core
// adds that the daemon does not emit shows up in it.
func assertMirrors(t *testing.T, name string, daemonValue, coreMirror any) {
	t.Helper()

	wire, err := json.Marshal(daemonValue)
	if err != nil {
		t.Fatalf("%s: marshal daemon payload: %v", name, err)
	}
	if err := json.Unmarshal(wire, coreMirror); err != nil {
		t.Fatalf("%s: core mirror cannot decode the daemon payload: %v", name, err)
	}
	mirrored, err := json.Marshal(coreMirror)
	if err != nil {
		t.Fatalf("%s: marshal core mirror: %v", name, err)
	}

	var sent, got any
	if err := json.Unmarshal(wire, &sent); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if err := json.Unmarshal(mirrored, &got); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !reflect.DeepEqual(sent, got) {
		t.Errorf("%s: the core mirror is not the daemon payload\n daemon sends: %s\n core holds:   %s", name, wire, mirrored)
	}
}

// stamp is a fixed non-zero time: every field in these fixtures is populated so
// an omitempty on one side and not the other cannot hide a mismatch.
var stamp = time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)

func TestSyncStatusPayloadMirrorsCoreModel(t *testing.T) {
	daemonValue := syncStatusResponse{
		Enabled:           true,
		Degraded:          true,
		ConfigError:       &ConfigDegradation{Code: "invalid_config", Message: "roots.bad", Recovery: "restart"},
		DBPath:            "/state/sync.db",
		MachineName:       "laptop",
		MachineID:         "01ARZ3NDEKTSV4RRFFQ69G5FAM",
		OriginID:          "origin-1",
		Server:            "https://sync.example.com",
		AuthError:         "token rejected",
		AuthErrorSince:    stamp,
		Documents:         42,
		DocumentsDiverged: 1,
		OutboxPending:     3,
		OutboxParked:      2,
		MigrationRequired: "sync.db predates the notespace id migration",
		Notespaces: []syncNotespaceStatus{{
			NotespaceID:   "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			NotespaceName: "notes",
			Cursor:        137,
			LastSyncedAt:  stamp,
			Hydration: &syncdb.HydrationProgress{
				Notespace:   "01ARZ3NDEKTSV4RRFFQ69G5FAV",
				Root:        "/Users/x/notebooks/nb/notespaces/notes",
				Running:     true,
				Scanned:     500,
				Enqueued:    12,
				Quarantined: 1,
				StartedAt:   stamp,
				FinishedAt:  stamp,
				FilesPerSec: 83.5,
			},
			Pull: true,
			Mode: "full",
			Role: "peer",
		}},
	}
	assertMirrors(t, "GET /api/sync/status", daemonValue, &models.SyncStatus{})
}

func TestSyncOutboxPayloadMirrorsCoreModel(t *testing.T) {
	daemonValue := syncOutboxResponse{
		ID:            7,
		DocumentID:    "doc-1",
		NotespaceID:   "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		NotespaceName: "notes",
		EventType:     "document_updated",
		Path:          "inbox/todo.md",
		PrevPath:      "inbox/old.md",
		ContentHash:   "abc123",
		CreatedAt:     stamp,
		Parked:        true,
		Attempts:      4,
		NextRetryAt:   stamp,
		ParkReason:    "secret_quarantine",
	}
	assertMirrors(t, "GET /api/sync/outbox", daemonValue, &models.SyncOutboxEntry{})
}

func TestSyncConflictPayloadMirrorsCoreModel(t *testing.T) {
	daemonValue := syncConflictResponse{
		NotespaceID:     "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		NotespaceName:   "notes",
		Path:            "plans/roadmap.md",
		DocumentID:      "doc-9",
		Kind:            "merge",
		Artifact:        "plans/roadmap.md.doc-9.conflict.md",
		ArtifactContent: "<<<",
		BaseContent:     "base",
	}
	assertMirrors(t, "GET /api/sync/conflicts", daemonValue, &models.SyncConflict{})
}

// The repush handler encodes a literal map rather than a struct, so the mirror
// is asserted against the same keys it writes.
func TestSyncRepushPayloadMirrorsCoreModel(t *testing.T) {
	daemonValue := map[string]any{
		"notespaces":      []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		"documents_reset": 12,
	}
	assertMirrors(t, "POST /api/sync/repush", daemonValue, &models.SyncRepushResult{})
}
