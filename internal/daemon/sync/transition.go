package sync

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrLegacySchema marks a name-keyed client database. Ordinary daemon startup
// must not mutate it: only grove migrate (step 2) may archive and rebuild it.
var ErrLegacySchema = errors.New("legacy name-keyed sync database; run grove migrate (step 2)")

// SchemaState is the non-mutating result of inspecting a client database.
type SchemaState struct {
	Exists bool
	Legacy bool
}

// InspectSchema opens an existing database read-only and classifies its key
// column. A missing or empty database is ready for the v2 schema.
func InspectSchema(path string) (SchemaState, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return SchemaState{}, nil
	} else if err != nil {
		return SchemaState{}, err
	}
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", path))
	if err != nil {
		return SchemaState{}, err
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(sync_documents)`)
	if err != nil {
		return SchemaState{}, fmt.Errorf("inspect sync database schema: %w", err)
	}
	defer rows.Close()
	var hasWorkspace, hasNotespace bool
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return SchemaState{}, err
		}
		hasWorkspace = hasWorkspace || name == "workspace"
		hasNotespace = hasNotespace || name == "notespace"
	}
	return SchemaState{Exists: true, Legacy: hasWorkspace && !hasNotespace}, rows.Err()
}

// RebuildReceipt is the WAL-safe archive/rebuild evidence consumed by the
// operator-only step-2 migration.
type RebuildReceipt struct {
	Database       string `json:"database"`
	Archive        string `json:"archive,omitempty"`
	Rebuilt        bool   `json:"rebuilt"`
	AlreadyCurrent bool   `json:"already_current"`
}

// ArchiveAndRebuild performs the explicit client transition. It checkpoints
// WAL before moving the complete database aside and creates a fresh id-keyed
// database; it never translates name keys into ids.
func ArchiveAndRebuild(path string) (RebuildReceipt, error) {
	state, err := InspectSchema(path)
	if err != nil {
		return RebuildReceipt{}, err
	}
	if !state.Exists || !state.Legacy {
		return RebuildReceipt{Database: path, AlreadyCurrent: true}, nil
	}
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_busy_timeout=5000", path))
	if err != nil {
		return RebuildReceipt{}, err
	}
	if _, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		return RebuildReceipt{}, fmt.Errorf("checkpoint legacy sync database: %w", err)
	}
	if err = db.Close(); err != nil {
		return RebuildReceipt{}, err
	}
	archive := path + ".legacy-v1-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	if err = os.Rename(path, archive); err != nil {
		return RebuildReceipt{}, fmt.Errorf("archive legacy sync database: %w", err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	fresh, err := Open(path)
	if err != nil {
		_ = os.Rename(archive, path)
		return RebuildReceipt{}, fmt.Errorf("create id-keyed sync database: %w", err)
	}
	if err = fresh.Close(); err != nil {
		return RebuildReceipt{}, err
	}
	return RebuildReceipt{Database: filepath.Clean(path), Archive: filepath.Clean(archive), Rebuilt: true}, nil
}
