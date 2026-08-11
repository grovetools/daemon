package sync

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyDetectionIsNonMutatingAndOpenRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sync_documents(document_id TEXT PRIMARY KEY, workspace TEXT NOT NULL, path TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	state, err := InspectSchema(path)
	if err != nil || !state.Legacy {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err := Open(path); !errors.Is(err, ErrLegacySchema) {
		t.Fatalf("Open err=%v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("ordinary detection mutated legacy database")
	}
}

func TestArchiveAndRebuildIsIdempotentAndWALSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE sync_documents(document_id TEXT PRIMARY KEY, workspace TEXT NOT NULL, path TEXT NOT NULL); INSERT INTO sync_documents VALUES('d','display-name','a.md')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	receipt, err := ArchiveAndRebuild(path)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Rebuilt || receipt.Archive == "" {
		t.Fatalf("receipt=%+v", receipt)
	}
	arch, err := sql.Open("sqlite3", receipt.Archive)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err = arch.QueryRow(`SELECT workspace FROM sync_documents WHERE document_id='d'`).Scan(&got); err != nil || got != "display-name" {
		t.Fatalf("archive row=%q err=%v", got, err)
	}
	arch.Close()
	state, err := InspectSchema(path)
	if err != nil || state.Legacy {
		t.Fatalf("fresh state=%+v err=%v", state, err)
	}
	again, err := ArchiveAndRebuild(path)
	if err != nil || !again.AlreadyCurrent || again.Rebuilt {
		t.Fatalf("again=%+v err=%v", again, err)
	}
}
