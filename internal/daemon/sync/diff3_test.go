package sync

import "testing"

func TestDiff3Merge(t *testing.T) {
	base := "line one\nline two\nline three\nline four\nline five\n"

	tests := []struct {
		name       string
		base       string
		local      string
		remote     string
		wantClean  bool
		wantMerged string // checked only when wantClean
	}{
		{
			name:       "no change either side",
			base:       base,
			local:      base,
			remote:     base,
			wantClean:  true,
			wantMerged: base,
		},
		{
			name:       "local-only change",
			base:       base,
			local:      "line one\nLOCAL two\nline three\nline four\nline five\n",
			remote:     base,
			wantClean:  true,
			wantMerged: "line one\nLOCAL two\nline three\nline four\nline five\n",
		},
		{
			name:       "remote-only change",
			base:       base,
			local:      base,
			remote:     "line one\nline two\nline three\nREMOTE four\nline five\n",
			wantClean:  true,
			wantMerged: "line one\nline two\nline three\nREMOTE four\nline five\n",
		},
		{
			name:       "disjoint changes both sides",
			base:       base,
			local:      "LOCAL one\nline two\nline three\nline four\nline five\n",
			remote:     "line one\nline two\nline three\nline four\nREMOTE five\n",
			wantClean:  true,
			wantMerged: "LOCAL one\nline two\nline three\nline four\nREMOTE five\n",
		},
		{
			name:       "adjacent but disjoint hunks",
			base:       base,
			local:      "line one\nLOCAL two\nline three\nline four\nline five\n",
			remote:     "line one\nline two\nREMOTE three\nline four\nline five\n",
			wantClean:  true,
			wantMerged: "line one\nLOCAL two\nREMOTE three\nline four\nline five\n",
		},
		{
			name:       "identical change both sides",
			base:       base,
			local:      "line one\nSAME two\nline three\nline four\nline five\n",
			remote:     "line one\nSAME two\nline three\nline four\nline five\n",
			wantClean:  true,
			wantMerged: "line one\nSAME two\nline three\nline four\nline five\n",
		},
		{
			name:      "overlapping change",
			base:      base,
			local:     "line one\nLOCAL two\nline three\nline four\nline five\n",
			remote:    "line one\nREMOTE two\nline three\nline four\nline five\n",
			wantClean: false,
		},
		{
			name:      "both append at end differently",
			base:      base,
			local:     base + "local tail\n",
			remote:    base + "remote tail\n",
			wantClean: false,
		},
		{
			name:       "both append at end identically",
			base:       base,
			local:      base + "same tail\n",
			remote:     base + "same tail\n",
			wantClean:  true,
			wantMerged: base + "same tail\n",
		},
		{
			name:      "one side deletes a region the other edits",
			base:      base,
			local:     "line one\nline four\nline five\n", // deleted two+three
			remote:    "line one\nline two\nEDITED three\nline four\nline five\n",
			wantClean: false,
		},
		{
			name:       "deletion composes with disjoint edit",
			base:       base,
			local:      "line one\nline four\nline five\n", // deleted two+three
			remote:     "line one\nline two\nline three\nline four\nREMOTE five\n",
			wantClean:  true,
			wantMerged: "line one\nline four\nREMOTE five\n",
		},
		{
			name:       "trailing newline removed locally, remote edits elsewhere",
			base:       "line one\nline two\n",
			local:      "line one\nline two", // stripped trailing newline
			remote:     "REMOTE one\nline two\n",
			wantClean:  true,
			wantMerged: "REMOTE one\nline two",
		},
		{
			name:      "trailing newline removed locally, remote edits last line",
			base:      "line one\nline two\n",
			local:     "line one\nline two",
			remote:    "line one\nREMOTE two\n",
			wantClean: false, // both changed the last line region differently
		},
		{
			name:       "insertion composes with edit after it",
			base:       base,
			local:      "line one\nINSERTED\nline two\nline three\nline four\nline five\n",
			remote:     "line one\nline two\nline three\nREMOTE four\nline five\n",
			wantClean:  true,
			wantMerged: "line one\nINSERTED\nline two\nline three\nREMOTE four\nline five\n",
		},
		{
			name:      "competing insertions at the same point",
			base:      base,
			local:     "line one\nLOCAL INSERT\nline two\nline three\nline four\nline five\n",
			remote:    "line one\nREMOTE INSERT\nline two\nline three\nline four\nline five\n",
			wantClean: false,
		},
		{
			name:       "empty base, only local adds content",
			base:       "",
			local:      "fresh content\n",
			remote:     "",
			wantClean:  true,
			wantMerged: "fresh content\n",
		},
		{
			name:      "empty base, both add different content",
			base:      "",
			local:     "local content\n",
			remote:    "remote content\n",
			wantClean: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, clean := diff3Merge([]byte(tt.base), []byte(tt.local), []byte(tt.remote))
			if clean != tt.wantClean {
				t.Fatalf("clean = %v, want %v (merged=%q)", clean, tt.wantClean, merged)
			}
			if !tt.wantClean {
				return
			}
			if string(merged) != tt.wantMerged {
				t.Fatalf("merged = %q, want %q", merged, tt.wantMerged)
			}
		})
	}
}

// TestDiff3MergeSymmetric: composing merges must be symmetric in content —
// swapping local and remote yields the same merged document for disjoint
// changes, and conflicts stay conflicts.
func TestDiff3MergeSymmetric(t *testing.T) {
	base := []byte("a\nb\nc\nd\n")
	local := []byte("A\nb\nc\nd\n")
	remote := []byte("a\nb\nc\nD\n")

	m1, ok1 := diff3Merge(base, local, remote)
	m2, ok2 := diff3Merge(base, remote, local)
	if !ok1 || !ok2 {
		t.Fatalf("expected clean merges, got %v/%v", ok1, ok2)
	}
	if string(m1) != string(m2) {
		t.Fatalf("merge not symmetric: %q vs %q", m1, m2)
	}
}
