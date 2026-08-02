package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/registry"

	// The REAL grove-syncd, in process. The daemon module does not require the
	// sync module in go.mod — it does not need to, because every grovetools
	// module is a main module of the repo's go.work, which is how the whole
	// ecosystem is built and tested. This is the only cross-module import in
	// the daemon and it is TEST-ONLY: nothing in the shipped daemon binary
	// links grove-syncd.
	syncserver "github.com/grovetools/sync/pkg/server"
	syncstore "github.com/grovetools/sync/pkg/store"
)

// This file is job 18's acceptance suite: two origins, one real server,
// exercising the presence registry end to end.
//
// The plan owner's amended timing note (STATE.md, "Job amendments") replaced
// the original "runs after the first two-machine deployment" gate with local
// two-origin acceptance, of which an in-process sync/pkg/server is the named
// first option. Validation against the real solair↔solm4 deployment remains a
// tracked follow-up, not a prerequisite.

const twoOriginToken = "registry-acceptance-token"

// startSyncServer boots the real grove-syncd handler over httptest with a
// hermetic sqlite store and blob dir.
func startSyncServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	st, err := syncstore.Open(filepath.Join(dir, "syncd.db"))
	if err != nil {
		t.Fatalf("syncstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sum := sha256.Sum256([]byte(twoOriginToken))
	if err := st.CreateToken(hex.EncodeToString(sum[:]), "acceptance", syncstore.OwnerUserID); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	blobs, err := syncstore.NewFSBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("NewFSBlobStore: %v", err)
	}

	ts := httptest.NewServer(syncserver.New(syncserver.Options{Store: st, Blobs: blobs}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// machineOrigin is one machine's whole client-side world: its own sync.db,
// its own origin id, its own machine id, its own local registry tree.
type machineOrigin struct {
	t         *testing.T
	machineID string
	root      string
	db        *DB
	client    *Client
	conflicts []string
}

func newMachineOrigin(t *testing.T, serverURL, machineID string) *machineOrigin {
	t.Helper()
	db := openTestDB(t)
	client := NewClient(ClientConfig{
		ServerURL: serverURL,
		Token:     twoOriginToken,
		// DeviceID is the durable machine identity; OriginID is the per-sync.db
		// install id. Distinct on purpose — the note records both so a wiped
		// sync.db reads as "same machine, new origin".
		DeviceID: machineID,
		OriginID: machineID + "-origin",
	})
	if _, err := client.Capabilities(context.Background(), "acceptance"); err != nil {
		t.Fatalf("capabilities handshake for %s: %v", machineID, err)
	}
	return &machineOrigin{t: t, machineID: machineID, root: t.TempDir(), db: db, client: client}
}

// publish writes a document into this machine's local tree and pushes it,
// exactly as the watcher's flush + push pipeline would.
func (m *machineOrigin) publish(relPath string, content []byte) {
	m.t.Helper()
	full := filepath.Join(m.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		m.t.Fatal(err)
	}
	reason, err := InsertAndEnqueue(m.db, "registry", relPath, content, time.Now())
	if err != nil {
		m.t.Fatalf("InsertAndEnqueue(%s): %v", relPath, err)
	}
	if reason != "" {
		m.t.Fatalf("document %s was quarantined as a secret (%s)", relPath, reason)
	}
	push := NewPushPipeline(m.db, m.client, "registry", logging.NewUnifiedLogger("test.push"), PushConfig{})
	for {
		n, err := push.DrainOutbox(context.Background(), m.root)
		if err != nil {
			m.t.Fatalf("DrainOutbox: %v", err)
		}
		if n == 0 {
			return
		}
	}
}

// pullOnce runs the REAL pull loop until the server has nothing more to give,
// so the guard is exercised where it actually lives (applyEvent, reached from
// RunPullLoop) rather than through a hand-rolled stand-in.
func (m *machineOrigin) pullOnce() {
	m.t.Helper()
	p := NewPullPipeline(
		&config.SyncWorkspace{Name: "registry", Role: config.SyncRoleRegistry, Pull: true},
		m.client, m.db, logging.NewUnifiedLogger("test.pull"))
	p.OwnMachineID = m.machineID
	p.OnRegistryForeignWrite = func(ws, path, detail string) {
		m.conflicts = append(m.conflicts, ws+" "+path)
	}
	p.pollWait = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	before, _ := m.db.GetWorkspaceCursor("registry")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.RunPullLoop(ctx, m.root)
	}()
	// The loop long-polls; stop as soon as the cursor stops moving.
	stable := 0
	for stable < 3 {
		time.Sleep(80 * time.Millisecond)
		cur, _ := m.db.GetWorkspaceCursor("registry")
		if cur == before {
			stable++
		} else {
			stable, before = 0, cur
		}
		if ctx.Err() != nil {
			break
		}
	}
	cancel()
	<-done
}

func (m *machineOrigin) noteBytes(machineID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(m.root, filepath.FromSlash(registry.NotePath(machineID))))
}

func sampleMachineNote(machineID, name string, rev int64) []byte {
	return (&registry.Note{
		MachineID:     machineID,
		Name:          name,
		Rev:           rev,
		LastSeen:      "2026-08-02",
		OriginID:      machineID + "-origin",
		GrovedVersion: "0.6.3",
		Ecosystems: []registry.NoteEcosystem{{
			Name: "grovetools", Path: "/code/grovetools",
			State: registry.StateDeclaredMissing, Enabled: true,
		}},
	}).Render()
}

// TestTwoOriginRegistryReplication is the acceptance test named in job 18:
// A's note replicates to B; B's inbound event for B's OWN path is dropped and
// surfaces as registry_foreign_write; OCC never fires on registry notes.
func TestTwoOriginRegistryReplication(t *testing.T) {
	// Conflict artifacts resolve through paths.StateDir(); sandbox it, or the
	// forged-write leg of this test would write into the developer's real
	// ~/.local/state/grove.
	t.Setenv("GROVE_HOME", t.TempDir())

	serverURL := startSyncServer(t)
	const idA = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	const idB = "01BBBBBBBBBBBBBBBBBBBBBBBB"
	a := newMachineOrigin(t, serverURL, idA)
	b := newMachineOrigin(t, serverURL, idB)

	// --- A's note replicates to B -----------------------------------------
	noteA := sampleMachineNote(idA, "solair", 1)
	a.publish(registry.NotePath(idA), noteA)

	b.pullOnce()
	got, err := b.noteBytes(idA)
	if err != nil {
		t.Fatalf("A's note did not replicate to B: %v", err)
	}
	if string(got) != string(noteA) {
		t.Fatalf("replicated note differs:\n--- A ---\n%s\n--- B ---\n%s", noteA, got)
	}
	// It parses on the reading side, which is what any surface will do.
	parsed, err := registry.ParseNote(got)
	if err != nil || parsed.MachineID != idA || parsed.Name != "solair" {
		t.Fatalf("replicated note does not parse as A's: %+v, %v", parsed, err)
	}
	if len(b.conflicts) != 0 {
		t.Fatalf("a peer's note raised a conflict: %v", b.conflicts)
	}

	// --- and B's note replicates back to A --------------------------------
	noteB := sampleMachineNote(idB, "solm4", 1)
	b.publish(registry.NotePath(idB), noteB)
	a.pullOnce()
	if got, err := a.noteBytes(idB); err != nil || string(got) != string(noteB) {
		t.Fatalf("B's note did not replicate to A: %v", err)
	}

	// --- successive revisions replicate; OCC never fires ------------------
	// Registry notes are single-writer, so per-document OCC can never reject
	// one: every push carries the base_version this machine last synced,
	// because nothing else ever advanced it.
	for rev := int64(2); rev <= 5; rev++ {
		a.publish(registry.NotePath(idA), sampleMachineNote(idA, "solair", rev))
	}
	b.pullOnce()
	got, err = b.noteBytes(idA)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = registry.ParseNote(got)
	if err != nil || parsed.Rev != 5 {
		t.Fatalf("B did not converge on A's rev 5: %+v, %v", parsed, err)
	}
	assertNoParkedOutbox(t, a, "A")
	assertNoParkedOutbox(t, b, "B")

	// --- B's own path, written by someone else ----------------------------
	// A forges a note at B's path. The server accepts it — under the interim
	// trust model every token is the owner and may write any path — so the
	// only defense is B refusing to apply it.
	forged := sampleMachineNote(idB, "impostor", 99)
	a.publish(registry.NotePath(idB), forged)

	beforeConflicts := len(b.conflicts)
	b.pullOnce()

	if len(b.conflicts) != beforeConflicts+1 {
		t.Fatalf("B did not surface the foreign write: %v", b.conflicts)
	}
	if !strings.Contains(b.conflicts[len(b.conflicts)-1], registry.NotePath(idB)) {
		t.Errorf("conflict does not name B's own note: %v", b.conflicts)
	}
	// B's own note on disk is untouched: the forgery never landed.
	if got, err := b.noteBytes(idB); err != nil || string(got) != string(noteB) {
		t.Fatalf("B's own note was overwritten by the foreign write:\n%s", got)
	}
	// And the evidence is on disk, carrying its kind.
	artifacts := conflictFiles(t, "registry")
	if len(artifacts) != 1 || !strings.Contains(artifacts[0], ConflictKindRegistryForeignWrite) {
		t.Fatalf("conflict artifacts = %v", artifacts)
	}

	// A, which is not the subject of the forged note, applies it like any
	// other document: the guard is about protecting your OWN note, not about
	// policing the registry.
	a.pullOnce()
	if got, err := a.noteBytes(idB); err != nil || string(got) != string(forged) {
		t.Errorf("A did not converge on the (forged) note it can see: %v", err)
	}
}

func assertNoParkedOutbox(t *testing.T, m *machineOrigin, label string) {
	t.Helper()
	entries, err := m.db.ListOutbox("registry", 0)
	if err != nil {
		t.Fatalf("%s: ListOutbox: %v", label, err)
	}
	for _, e := range entries {
		if e.Parked {
			t.Errorf("%s: registry note parked (OCC or push rejection): path=%s reason=%s",
				label, e.Path, e.ParkReason)
		}
	}
	if len(entries) != 0 {
		t.Errorf("%s: %d outbox entries left undrained: %+v", label, len(entries), entries)
	}
}
