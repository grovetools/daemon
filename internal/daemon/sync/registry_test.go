package sync

import (
	"path/filepath"
	"testing"
)

func TestDisplayRenameDoesNotRekeyNotespaceState(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	first := NotespaceBinding{ID: id, Name: "before", Root: "/notes/before", Subject: "local:01ARZ3NDEKTSV4RRFFQ69G5FAW", Kind: "notes"}
	if err := db.UpsertNotespaceBinding(first); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCursor(id, 42); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertDocument(&Document{DocumentID: "document-1", Notespace: id, Path: "a.md"}); err != nil {
		t.Fatal(err)
	}

	renamed := first
	renamed.Name = "after"
	renamed.Root = "/notes/after"
	if err := db.UpsertNotespaceBinding(renamed); err != nil {
		t.Fatal(err)
	}
	binding, err := db.GetNotespaceBinding(id)
	if err != nil || binding == nil || binding.Name != "after" || binding.Root != "/notes/after" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	state, err := db.GetState(id)
	if err != nil || state == nil || state.Cursor != 42 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	doc, err := db.GetDocumentByPath(id, "a.md")
	if err != nil || doc == nil || doc.DocumentID != "document-1" {
		t.Fatalf("document=%+v err=%v", doc, err)
	}
	if old, err := db.GetState("before"); err != nil || old != nil {
		t.Fatalf("display name became a state key: state=%+v err=%v", old, err)
	}
}
