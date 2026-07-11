package sync

import (
	"testing"

	"github.com/grovetools/core/config"
)

// TestDocSpaceIncluded ports the former watcher-package TestSyncExclusionManifest
// table (which tested syncExcluded directly) onto DocSpace.Included — the
// exclusion logic now lives here.
func TestDocSpaceIncluded(t *testing.T) {
	cases := []struct {
		rel      string
		extra    []string
		excluded bool
	}{
		// Default exclusion manifest.
		{rel: ".obsidian/workspace.json", excluded: true},
		{rel: "notes/.obsidian/app.json", excluded: true},
		{rel: ".stfolder", excluded: true},
		{rel: ".stversions/old.md", excluded: true},
		{rel: "notes/a.sync-conflict-20240101-ABCDEF.md", excluded: true},
		{rel: "notes/a.conflict.md", excluded: true},
		{rel: ".grove/rules", excluded: true},
		{rel: ".grove/rules/extra.rules", excluded: true},
		{rel: ".cx/state.json", excluded: true},
		{rel: ".git/objects/ab/cdef", excluded: true},
		{rel: "notes/.git/config", excluded: true},
		{rel: "plans/my-plan/.artifacts/briefing.xml", excluded: true},
		{rel: "plans/my-plan.lock", excluded: true},
		{rel: ".DS_Store", excluded: true},
		{rel: "notes/.DS_Store", excluded: true},
		// Allowed content.
		{rel: "notes/inbox/idea.md", excluded: false},
		{rel: "plans/my-plan/01-spec.md", excluded: false},
		{rel: "chats/session.md", excluded: false},
		{rel: ".grove/other", excluded: false},
		// Per-workspace extra globs.
		{rel: "notes/secret-draft.md", extra: []string{"*-draft.md"}, excluded: true},
		{rel: "private/x.md", extra: []string{"private/"}, excluded: true},
		{rel: "notes/public.md", extra: []string{"private/"}, excluded: false},
	}

	for _, tc := range cases {
		d := NewDocSpace(&config.SyncWorkspace{Excludes: tc.extra})
		if got := d.Included(tc.rel); got != !tc.excluded {
			t.Errorf("Included(%q) with excludes %v = %v, want %v", tc.rel, tc.extra, got, !tc.excluded)
		}
	}
}

// TestDocSpaceRoute covers the size-based routing decisions on top of the
// inclusion filter, plus the per-workspace MaxFileSize cap.
func TestDocSpaceRoute(t *testing.T) {
	const kb = 1 << 10
	cases := []struct {
		name string
		ws   *config.SyncWorkspace
		rel  string
		size int64
		want RouteDecision
	}{
		{name: "excluded dir", rel: ".obsidian/x.md", size: 10, want: RouteSkip},
		{name: "excluded artifacts", rel: ".artifacts/log.txt", size: 10, want: RouteSkip},
		{name: "excluded sync-conflict", rel: "note.sync-conflict-123.md", size: 10, want: RouteSkip},
		{name: "excluded conflict suffix", rel: "foo.conflict.md", size: 10, want: RouteSkip},
		{name: "excluded lock", rel: "x.lock", size: 10, want: RouteSkip},
		{name: "excluded grove rules", rel: ".grove/rules/a.rules", size: 10, want: RouteSkip},
		{name: "small inline", rel: "quick/a.md", size: 4 * kb, want: RouteInline},
		{name: "at boundary inline", rel: "quick/a.md", size: 256 * kb, want: RouteInline},
		{name: "over boundary blob", rel: "quick/a.md", size: 300 * kb, want: RouteBlob},
		{
			name: "glob exclude",
			ws:   &config.SyncWorkspace{Excludes: []string{"daily/*"}},
			rel:  "daily/2026-07-11.md", size: 10, want: RouteSkip,
		},
		{
			name: "over per-workspace cap",
			ws:   &config.SyncWorkspace{MaxFileSize: 1024},
			rel:  "quick/big.md", size: 2 * kb, want: RouteSkip,
		},
		{
			name: "under per-workspace cap",
			ws:   &config.SyncWorkspace{MaxFileSize: 1024},
			rel:  "quick/ok.md", size: 512, want: RouteInline,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDocSpace(tc.ws)
			if got := d.Route(tc.rel, tc.size); got != tc.want {
				t.Errorf("Route(%q, %d) = %v, want %v", tc.rel, tc.size, got, tc.want)
			}
			// Included consistency: any RouteSkip caused purely by exclusion
			// (not the size cap) must agree with Included==false.
			if tc.want == RouteSkip && (tc.ws == nil || tc.ws.MaxFileSize == 0) {
				if d.Included(tc.rel) {
					t.Errorf("Included(%q) = true but Route skipped it", tc.rel)
				}
			}
			if tc.want != RouteSkip && !d.Included(tc.rel) {
				t.Errorf("Included(%q) = false but Route did not skip it", tc.rel)
			}
		})
	}
}

// TestNewDocSpaceNil verifies the nil-subscription all-defaults instance is
// usable (the watcher test handler and any pre-subscription path rely on it).
func TestNewDocSpaceNil(t *testing.T) {
	d := NewDocSpace(nil)
	if !d.Included("notes/a.md") {
		t.Fatal("nil DocSpace should include ordinary paths")
	}
	if d.Route("notes/a.md", 300<<10) != RouteBlob {
		t.Fatal("nil DocSpace should route a 300KB file to the blob tier")
	}
	if d.Included(".artifacts/x") {
		t.Fatal("nil DocSpace should still apply the default exclusion manifest")
	}
}
