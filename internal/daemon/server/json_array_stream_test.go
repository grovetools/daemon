package server

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

// TestWriteJSONArrayStreamedMatchesEncode pins the whole point of the streamed
// writer: it must be byte-for-byte interchangeable with the buffering encoder
// AS PARSED. It is not byte-identical (each element carries its own newline),
// so the check is on the decoded value, which is what every client sees.
func TestWriteJSONArrayStreamedMatchesEncode(t *testing.T) {
	entries := []*models.NoteIndexEntry{
		{
			Path: "/nb/notes/inbox/a.md", Name: "a.md", Title: "Alpha",
			Tags: []string{"perf", "heap"}, ID: "n1", PlanRef: "plans/x",
			PlanJob: "01-do.md", Priority: "p0",
			Created: time.Unix(1700000000, 0).UTC(), ModTime: time.Unix(1700000001, 0).UTC(),
			Type: "note", Group: "inbox", Workspace: "grovetools", ContentDir: "notes",
		},
		// A title with characters the string encoder must escape, so the
		// per-element path is exercised on the same code the bulk path used.
		{Path: `/nb/notes/b"\.md`, Name: "b.md", Title: "quote \" and \\ and <tag> and é",
			ModTime: time.Unix(1700000002, 0).UTC(), Type: "generic", Group: "quick"},
	}

	var streamed bytes.Buffer
	writeJSONArrayStreamed(&streamed, entries)

	var bulk bytes.Buffer
	if err := json.NewEncoder(&bulk).Encode(entries); err != nil {
		t.Fatalf("bulk encode: %v", err)
	}

	var gotStreamed, gotBulk []*models.NoteIndexEntry
	if err := json.Unmarshal(streamed.Bytes(), &gotStreamed); err != nil {
		t.Fatalf("streamed output does not parse as JSON: %v\n%s", err, streamed.String())
	}
	if err := json.Unmarshal(bulk.Bytes(), &gotBulk); err != nil {
		t.Fatalf("bulk parse: %v", err)
	}
	if !reflect.DeepEqual(gotStreamed, gotBulk) {
		t.Errorf("streamed and bulk encodings decode differently:\n streamed=%+v\n bulk=%+v", gotStreamed, gotBulk)
	}
}

// An empty index must stay an empty ARRAY. json.Encode on a nil slice writes
// "null", and clients that range over the response would break on it.
func TestWriteJSONArrayStreamedEmptyIsArray(t *testing.T) {
	for name, in := range map[string][]*models.NoteIndexEntry{
		"nil":   nil,
		"empty": {},
	} {
		var buf bytes.Buffer
		writeJSONArrayStreamed(&buf, in)
		if got := buf.String(); got != "[]" {
			t.Errorf("%s slice encoded as %q, want %q", name, got, "[]")
		}
		var back []*models.NoteIndexEntry
		if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
			t.Errorf("%s slice output does not parse: %v", name, err)
		}
	}
}
