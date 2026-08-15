package sync

import (
	"fmt"
	"testing"
)

func TestActivityRecordAndList(t *testing.T) {
	db := openTestDB(t)

	if err := db.RecordActivity(&ActivityEntry{
		Notespace: "ns-a", Direction: ActivityOutgoing, EventType: "document_updated",
		Path: "notes/a.md", Result: ActivityResultSynced, Version: 3,
	}); err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}
	if err := db.RecordActivity(&ActivityEntry{
		Notespace: "ns-b", Direction: ActivityIncoming, EventType: "document_created",
		Path: "inbox/b.md", DocumentID: "doc-b", Result: ActivityResultApplied, Version: 1,
	}); err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}

	// Newest-first across all notespaces.
	all, err := db.ListActivity("", 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(all))
	}
	if all[0].Path != "inbox/b.md" || all[1].Path != "notes/a.md" {
		t.Fatalf("expected newest-first order, got %q then %q", all[0].Path, all[1].Path)
	}
	if all[0].Direction != ActivityIncoming || all[0].Result != ActivityResultApplied {
		t.Fatalf("unexpected first entry: %+v", all[0])
	}
	if all[0].OccurredAt.IsZero() {
		t.Fatal("expected occurred_at to be stamped")
	}

	// Notespace filter and limit.
	only, err := db.ListActivity("ns-a", 0)
	if err != nil {
		t.Fatalf("ListActivity(ns-a): %v", err)
	}
	if len(only) != 1 || only[0].Notespace != "ns-a" {
		t.Fatalf("expected only ns-a entries, got %+v", only)
	}
	limited, err := db.ListActivity("", 1)
	if err != nil {
		t.Fatalf("ListActivity(limit): %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected 1 entry under limit, got %d", len(limited))
	}
}

func TestActivityPrunesToCap(t *testing.T) {
	db := openTestDB(t)

	for i := 0; i < activityCap+25; i++ {
		if err := db.RecordActivity(&ActivityEntry{
			Notespace: "ns", Direction: ActivityOutgoing, EventType: "document_updated",
			Path: fmt.Sprintf("n/%d.md", i), Result: ActivityResultSynced,
		}); err != nil {
			t.Fatalf("RecordActivity(%d): %v", i, err)
		}
	}

	all, err := db.ListActivity("", 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(all) != activityCap {
		t.Fatalf("expected the table pruned to %d rows, got %d", activityCap, len(all))
	}
	// The survivors are the newest rows.
	if want := fmt.Sprintf("n/%d.md", activityCap+24); all[0].Path != want {
		t.Fatalf("expected newest entry %q first, got %q", want, all[0].Path)
	}
}
