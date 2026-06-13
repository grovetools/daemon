package sync

import (
	"strings"
	"testing"
)

// TestMergeValuesBothChangedTiebreak pins the LWW rule for both-changed
// frontmatter keys: the side whose `modified:` timestamp parses later wins;
// missing/unparseable/equal `modified:` prefers local. Frontmatter never
// parks a document — every key resolves.
func TestMergeValuesBothChangedTiebreak(t *testing.T) {
	tests := []struct {
		name      string
		base      Frontmatter
		local     Frontmatter
		remote    Frontmatter
		key       string
		wantValue interface{}
	}{
		{
			name:      "remote modified later: remote wins",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "local title", "modified": "2026-06-12 11:00:00"},
			remote:    Frontmatter{"title": "remote title", "modified": "2026-06-12 12:00:00"},
			key:       "title",
			wantValue: "remote title",
		},
		{
			name:      "local modified later: local wins",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "local title", "modified": "2026-06-12 12:00:00"},
			remote:    Frontmatter{"title": "remote title", "modified": "2026-06-12 11:00:00"},
			key:       "title",
			wantValue: "local title",
		},
		{
			name:      "equal modified: local wins",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "local title", "modified": "2026-06-12 11:00:00"},
			remote:    Frontmatter{"title": "remote title", "modified": "2026-06-12 11:00:00"},
			key:       "title",
			wantValue: "local title",
		},
		{
			name:      "missing modified: local wins",
			base:      Frontmatter{"title": "old"},
			local:     Frontmatter{"title": "local title"},
			remote:    Frontmatter{"title": "remote title"},
			key:       "title",
			wantValue: "local title",
		},
		{
			name:      "unparseable remote modified: local wins",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "local title", "modified": "2026-06-12 11:00:00"},
			remote:    Frontmatter{"title": "remote title", "modified": "not a timestamp"},
			key:       "title",
			wantValue: "local title",
		},
		{
			name:      "only local changed: local taken regardless of modified",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "local title", "modified": "2026-06-12 10:00:00"},
			remote:    Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			key:       "title",
			wantValue: "local title",
		},
		{
			name:      "only remote changed: remote taken regardless of modified",
			base:      Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			local:     Frontmatter{"title": "old", "modified": "2026-06-12 10:00:00"},
			remote:    Frontmatter{"title": "remote title", "modified": "2026-06-12 09:00:00"},
			key:       "title",
			wantValue: "remote title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged := mergeValues(tt.base, tt.local, tt.remote)
			if got := merged[tt.key]; got != tt.wantValue {
				t.Fatalf("merged[%q] = %v, want %v", tt.key, got, tt.wantValue)
			}
		})
	}
}

// TestMergeValuesDeletionWins: when the winning side deleted a key, the key
// is dropped from the merge (not emitted as a literal nil).
func TestMergeValuesDeletionWins(t *testing.T) {
	base := Frontmatter{"tags": "[a]", "title": "x"}
	local := Frontmatter{"title": "x"} // local deleted tags
	remote := Frontmatter{"tags": "[a]", "title": "x"}

	merged := mergeValues(base, local, remote)
	if _, ok := merged["tags"]; ok {
		t.Fatalf("deleted key resurfaced in merge: %v", merged["tags"])
	}

	doc := reconstructDocument(merged, frontmatterKeys([]byte("---\ntitle: x\n---\nbody\n")), []byte("body\n"))
	if strings.Contains(string(doc), "<nil>") {
		t.Fatalf("reconstructed document leaked a nil value: %q", doc)
	}
}

// TestReconstructDocumentDeterministicOrder: output preserves the order hint
// and is byte-stable across calls (map iteration order must not leak).
func TestReconstructDocumentDeterministicOrder(t *testing.T) {
	fm := Frontmatter{"title": "t", "id": "1", "tags": "[x]", "extra": "e"}
	order := []string{"id", "title", "tags"}
	body := []byte("body\n")

	first := string(reconstructDocument(fm, order, body))
	for i := 0; i < 10; i++ {
		if got := string(reconstructDocument(fm, order, body)); got != first {
			t.Fatalf("non-deterministic reconstruction:\n%q\nvs\n%q", got, first)
		}
	}
	want := "---\nid: 1\ntitle: t\ntags: [x]\nextra: e\n---\nbody\n"
	if first != want {
		t.Fatalf("reconstructed = %q, want %q", first, want)
	}
}
