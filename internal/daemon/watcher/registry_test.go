package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/registry"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// registryFixture builds a hermetic machine: a sandboxed GROVE_HOME (which
// redirects config, state AND data, so the identity mint below can never reach
// the developer's real ~/.local/state/grove), a notebook definition, a
// registry-role subscription, and a materialized notespace root.
//
// Returns the handler, the registry notespace root, and this machine's id.
func registryFixture(t *testing.T, machineTOML string) (*SyncHandler, string, string) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("GROVE_HOME", home)
	config.ResetLoadCache()
	t.Cleanup(config.ResetLoadCache)

	configDir := filepath.Join(home, "config", "grove")
	notebookRoot := filepath.Join(home, "notebooks", "nb")
	// syntheticNodeFor prefers a notebook definition whose resolved notespace
	// root already exists on disk, so precreate it — this is what a `grove
	// join` does when it seeds the registry dirs.
	if err := os.MkdirAll(filepath.Join(notebookRoot, "notespaces", "registry", "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(configDir, "notebooks.toml"), `default = "nb"

[notebooks.nb]
root = "`+notebookRoot+`"
`)
	if machineTOML != "" {
		writeTestFile(t, filepath.Join(configDir, "machine.toml"), machineTOML)
	}

	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	syncCfg := &config.SyncConfig{
		Server: "http://127.0.0.1:0",
		Workspaces: []config.SyncWorkspace{
			{Name: "registry", Role: config.SyncRoleRegistry, Pull: true},
		},
	}

	db, err := syncdb.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("open sync db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := NewSyncHandler(nil, cfg, syncCfg, db, 50, 500)
	node, err := h.syntheticNodeFor("registry")
	if err != nil {
		t.Fatal(err)
	}
	root, err := h.nodeNotespaceRoot(node)
	if err != nil {
		t.Fatalf("fixture could not resolve the registry notespace root: %v", err)
	}

	id := machine.ID()
	if id == "" {
		t.Fatal("fixture could not mint a machine identity")
	}
	// Prove the sandbox held: the id must live under the temp GROVE_HOME.
	if p := machine.IdentityPath(); !filepath.HasPrefix(p, home) {
		t.Fatalf("machine identity escaped the sandbox: %s", p)
	}
	return h, root, id
}

func readNote(t *testing.T, root, id string) (*registry.Note, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(registry.NotePath(id))))
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	note, err := registry.ParseNote(data)
	if err != nil {
		t.Fatalf("parse note: %v\n%s", err, data)
	}
	return note, data
}

// TestRegistryWriterSuppressesNoOpTicks is the acceptance criterion: a tick
// that finds nothing changed must write NOTHING — not the same bytes again,
// not a bumped rev. Source-side suppression is what keeps the steady-state
// cost at one event per machine per day rather than one per tick.
func TestRegistryWriterSuppressesNoOpTicks(t *testing.T) {
	h, root, id := registryFixture(t, `[machine]
name = "fixture-a"
`)
	day := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	h.registryNow = func() time.Time { return day }

	h.writeRegistryNote(t.Context())
	note, first := readNote(t, root, id)
	if note.Rev != 1 {
		t.Fatalf("first write rev = %d, want 1", note.Rev)
	}
	if note.LastSeen != "2026-08-02" {
		t.Fatalf("last_seen = %q", note.LastSeen)
	}
	if note.MachineID != id {
		t.Fatalf("note machine_id = %q, want %q", note.MachineID, id)
	}
	if note.Name != "fixture-a" {
		t.Fatalf("note name = %q", note.Name)
	}

	notePath := filepath.Join(root, filepath.FromSlash(registry.NotePath(id)))
	before, err := os.Stat(notePath)
	if err != nil {
		t.Fatal(err)
	}

	// Ten no-op ticks on the same day.
	for i := 0; i < 10; i++ {
		h.writeRegistryNote(t.Context())
	}
	after, err := os.Stat(notePath)
	if err != nil {
		t.Fatal(err)
	}
	_, second := readNote(t, root, id)
	if string(first) != string(second) {
		t.Errorf("no-op ticks changed the note:\n--- before ---\n%s\n--- after ---\n%s", first, second)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("no-op tick rewrote the file (mtime %v -> %v)", before.ModTime(), after.ModTime())
	}
}

// TestRegistryWriterBumpsOnDayBoundary: last_seen is the liveness signal, so
// it (and rev with it) advances when the day rolls over even though nothing
// else about the machine changed.
func TestRegistryWriterBumpsOnDayBoundary(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	now := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	h.registryNow = func() time.Time { return now }

	h.writeRegistryNote(t.Context())
	if note, _ := readNote(t, root, id); note.Rev != 1 {
		t.Fatalf("rev = %d", note.Rev)
	}

	now = now.Add(26 * time.Hour) // next day
	h.writeRegistryNote(t.Context())
	note, _ := readNote(t, root, id)
	if note.Rev != 2 {
		t.Errorf("rev after a day boundary = %d, want 2", note.Rev)
	}
	if note.LastSeen != "2026-08-03" {
		t.Errorf("last_seen = %q, want 2026-08-03", note.LastSeen)
	}

	// ...and then goes quiet again on the same day.
	h.writeRegistryNote(t.Context())
	if again, _ := readNote(t, root, id); again.Rev != 2 {
		t.Errorf("rev after a same-day tick = %d, want 2", again.Rev)
	}
}

// TestRegistryWriterBumpsOnStructuralChange: a subscription appearing (or an
// ecosystem going missing) is exactly what other machines need to see.
func TestRegistryWriterBumpsOnStructuralChange(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	day := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	h.registryNow = func() time.Time { return day }

	h.writeRegistryNote(t.Context())
	note, _ := readNote(t, root, id)
	if len(note.Ecosystems) != 0 {
		t.Fatalf("expected no ecosystems: %+v", note.Ecosystems)
	}

	// Declare a specific recorded root that is NOT on disk: the
	// declared-missing state is the materialization verb's input and must reach
	// the note.
	configDir := filepath.Join(os.Getenv("GROVE_HOME"), "config", "grove")
	writeTestFile(t, filepath.Join(configDir, "notebooks.toml"),
		"default = \"nb\"\n[notebooks.nb]\nroot = \"/notes\"\n")
	writeTestFile(t, filepath.Join(configDir, "roots.toml"),
		"[roots.grovetools]\npath = \"/nonexistent/grovetools\"\n")

	h.writeRegistryNote(t.Context())
	note, _ = readNote(t, root, id)
	if note.Rev != 2 {
		t.Errorf("rev after a structural change = %d, want 2", note.Rev)
	}
	if len(note.Ecosystems) != 1 {
		t.Fatalf("ecosystem did not reach the note: %+v", note.Ecosystems)
	}
	if got := note.Ecosystems[0]; got.Name != "grovetools" || got.State != registry.StateDeclaredMissing {
		t.Errorf("ecosystem = %+v, want grovetools/declared-missing", got)
	}
}

// TestRegistryWriterRecordsSubscriptionsWithoutSecrets: the note replicates to
// every device, and the push path quarantines anything token-shaped — a leak
// here would take the machine's whole presence dark, not merely leak.
func TestRegistryWriterRecordsSubscriptionsWithoutSecrets(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	h.syncCfgMu.Lock()
	h.syncCfg.Token = "ghp_thisisnotarealtokenbutlookslikeone123456"
	h.syncCfg.TokenCommand = "pass show sync/token"
	h.syncCfgMu.Unlock()

	h.writeRegistryNote(t.Context())
	note, raw := readNote(t, root, id)
	if len(note.Subscriptions) != 1 || note.Subscriptions[0].Role != config.SyncRoleRegistry {
		t.Fatalf("subscriptions = %+v", note.Subscriptions)
	}
	if reason, found := syncdb.ScanForSecrets(raw); found {
		t.Fatalf("note would be quarantined as a secret (%s):\n%s", reason, raw)
	}
	for _, forbidden := range []string{"ghp_", "pass show", "token"} {
		if containsFold(string(raw), forbidden) {
			t.Errorf("note contains %q:\n%s", forbidden, raw)
		}
	}
}

func TestRegistryWriterIsDarkWithoutARegistrySubscription(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	h.syncCfgMu.Lock()
	// A push-only legacy entry: same notespace name, no role. The ROLE is what
	// makes a notespace the registry, never the name.
	h.syncCfg.Workspaces = []config.SyncWorkspace{{Name: "registry"}}
	h.syncCfgMu.Unlock()

	h.writeRegistryNote(t.Context())
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(registry.NotePath(id)))); err == nil {
		t.Error("a role-less subscription produced a presence note")
	}
}

// TestRegistryWriterRepairsACorruptedOwnNote: the writer owns this document,
// so an unparseable one is repaired rather than left to poison every reader.
func TestRegistryWriterRepairsACorruptedOwnNote(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	notePath := filepath.Join(root, filepath.FromSlash(registry.NotePath(id)))
	if err := os.MkdirAll(filepath.Dir(notePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notePath, []byte("garbage, not a note"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.writeRegistryNote(t.Context())
	note, _ := readNote(t, root, id)
	if note.MachineID != id {
		t.Errorf("corrupted note not repaired: %+v", note)
	}
}

// TestRegistryWriteIsAtomicAndUnsyncable: the intermediate file must be
// dot-prefixed, because MatchesEvent drops dot-prefixed basenames and that is
// what keeps a half-written note out of the outbox.
func TestRegistryWriteIsAtomicAndUnsyncable(t *testing.T) {
	h, root, id := registryFixture(t, "[machine]\nname = \"fixture-a\"\n")
	h.writeRegistryNote(t.Context())

	entries, err := os.ReadDir(filepath.Join(root, registry.MachinesDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != id+registry.NoteExt {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("machines dir = %v, want exactly the note", names)
	}
}

func containsFold(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexFold(haystack, needle) >= 0
}

func indexFold(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// The presence note is the one write this handler makes into a notebook, and
// writeNoteAtomically MkdirAlls its parent chain. Under a missing notespace
// root that would resurrect the whole notebook tree — so the writer refuses
// (Phase 3, W3.2) and leaves the recorded route for the operator to repair.
func TestRegistryWriterRefusesAMissingNotespaceRoot(t *testing.T) {
	h, root, id := registryFixture(t, `[machine]
name = "fixture-a"
`)
	// The note writes normally while the root is there.
	h.writeRegistryNote(t.Context())
	readNote(t, root, id)

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	h.registryWarned = false
	h.writeRegistryNote(t.Context())

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the registry writer recreated the notespace root: %v", err)
	}
	if !h.registryWarned {
		t.Fatal("the refusal was not surfaced to the operator")
	}
}
