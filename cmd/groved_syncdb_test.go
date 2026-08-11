package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
	_ "github.com/mattn/go-sqlite3"
)

func TestRootRegistersSyncDBArchiveRebuildCommand(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"sync-db-archive-rebuild"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd == rootCmd || cmd.Name() != "sync-db-archive-rebuild" {
		t.Fatalf("sync-db-archive-rebuild command is not registered on the shipped root")
	}
}

func TestSyncDBArchiveRebuildCommandEmitsReceipt(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "sync.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sync_documents(document_id TEXT PRIMARY KEY, workspace TEXT NOT NULL, path TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := newGrovedSyncDBCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--yes", "--path", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var receipt syncdb.RebuildReceipt
	if err := json.NewDecoder(&out).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Rebuilt || receipt.Archive == "" || receipt.Database != path {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestSyncDBArchiveRebuildCommandRequiresConfirmation(t *testing.T) {
	cmd := newGrovedSyncDBCmd()
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected --yes refusal")
	}
}
