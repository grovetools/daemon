package sync

// The loop half of the W3.5 outbound veto (the pipeline-birth race).
//
// The gate was consulted when a pipeline was BORN and again when the daemon
// next reconciled. Between those two moments the push loop of the pipeline
// whose own pull loop had just raised the verdict kept draining: in the field
// that window ran 9-20 seconds and it published the disputed tree to the
// server, which is precisely the decision W3.5 exists to defer to the operator.
//
// These tests pin the two moments a running loop can be stopped at — before it
// fetches a batch, and with a batch already in hand — without any timing: the
// gate is a function, so the test decides exactly when it closes.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/syncproto"
)

// withheldFixture is one queued outbox entry, a local file to push, and a
// server that records every push it is asked to accept.
func withheldFixture(t *testing.T) (*DB, string, *PushPipeline, *atomic.Int64) {
	t.Helper()
	db := openTestDB(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plans", "x.md"), []byte("machine B's plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueOutbox(&OutboxEntry{
		DocumentID:  "doc-1",
		Notespace:   "alpha",
		EventType:   syncproto.EventDocumentCreated,
		Path:        "plans/x.md",
		ContentHash: hashContent([]byte("machine B's plan")),
	}); err != nil {
		t.Fatalf("EnqueueOutbox: %v", err)
	}

	var pushes atomic.Int64
	srv := servePushStub(t, func(req *syncproto.PushRequest) *syncproto.PushResponse {
		pushes.Add(1)
		resp := &syncproto.PushResponse{Results: make([]syncproto.PushResult, len(req.Events))}
		for i := range resp.Results {
			resp.Results[i] = syncproto.PushResult{
				Status: syncproto.PushStatusAccepted, DocumentID: "doc-1", Version: 1,
			}
		}
		return resp
	})
	t.Cleanup(srv.Close)

	client := NewClient(ClientConfig{ServerURL: srv.URL, Token: "t", OriginID: db.OriginID()})
	handshake(t, client)
	pipeline := NewPushPipeline(db, client, "alpha", logging.NewUnifiedLogger("test.push"), PushConfig{})
	return db, root, pipeline, &pushes
}

// assertParked is the other half of every assertion here: withholding a push
// must PARK the local work, never discard it. Adoption is what releases it.
func assertParked(t *testing.T, db *DB) {
	t.Helper()
	remaining, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("the withheld entry left the outbox (%d remaining); withholding a push must park local work, not drop it", remaining)
	}
}

// A notespace contested before the drain starts sends nothing at all.
func TestDrainOutboxSendsNothingWhileTheNotespaceIsContested(t *testing.T) {
	db, root, pipeline, pushes := withheldFixture(t)
	pipeline.Withheld = func() bool { return true }

	n, err := pipeline.DrainOutbox(context.Background(), root)
	if err != nil {
		t.Fatalf("a veto is not a failure: DrainOutbox = %v", err)
	}
	if n != 0 {
		t.Fatalf("DrainOutbox acknowledged %d document(s) for a contested notespace", n)
	}
	if got := pushes.Load(); got != 0 {
		t.Fatalf("a contested notespace pushed %d batch(es) to the server", got)
	}
	assertParked(t, db)
}

// And the race the ticket is about: the verdict lands AFTER the batch has been
// fetched, read from disk and turned into events — the state the push loop of a
// contesting pipeline is in when its own pull loop calls OnContested.
//
// The gate is asked once per checkpoint, so returning false exactly once puts
// the verdict in the narrowest possible window: the batch is already in hand,
// and the very next thing DrainOutbox would do is send it. Before the loop
// consulted the gate at all, this batch went to the server and the contest was
// decided in favour of whichever machine happened to be pushing.
func TestDrainOutboxAbandonsAFetchedBatchWhenTheGateCloses(t *testing.T) {
	db, root, pipeline, pushes := withheldFixture(t)

	var checks atomic.Int64
	pipeline.Withheld = func() bool {
		// False for the pre-fetch check only; contested from then on.
		return checks.Add(1) > 1
	}

	n, err := pipeline.DrainOutbox(context.Background(), root)
	if err != nil {
		t.Fatalf("a veto is not a failure: DrainOutbox = %v", err)
	}
	if n != 0 {
		t.Fatalf("DrainOutbox acknowledged %d document(s) after the gate closed", n)
	}
	if got := pushes.Load(); got != 0 {
		t.Fatalf("the batch in hand was pushed anyway (%d batch(es)); the verdict must beat the send, not the next reconcile", got)
	}
	if got := checks.Load(); got < 2 {
		t.Fatalf("the gate was consulted %d time(s): a batch already fetched was sent without re-asking", got)
	}
	assertParked(t, db)
}

// The gate is live in both directions: it is read per batch, so clearing it
// (adoption) releases the parked work on the next pass with no restart and no
// re-plumbing.
func TestDrainOutboxResumesWhenTheVerdictClears(t *testing.T) {
	db, root, pipeline, pushes := withheldFixture(t)

	var contested atomic.Bool
	contested.Store(true)
	pipeline.Withheld = contested.Load

	if _, err := pipeline.DrainOutbox(context.Background(), root); err != nil {
		t.Fatalf("DrainOutbox: %v", err)
	}
	if got := pushes.Load(); got != 0 {
		t.Fatalf("precondition: the contested notespace already pushed %d batch(es)", got)
	}

	contested.Store(false) // adoption
	if _, err := pipeline.DrainOutbox(context.Background(), root); err != nil {
		t.Fatalf("DrainOutbox after adoption: %v", err)
	}
	if got := pushes.Load(); got != 1 {
		t.Fatalf("adoption released %d push(es), want the parked work to go out exactly once", got)
	}
	remaining, err := db.CountOutbox()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d entry(ies) still queued after adoption drained them", remaining)
	}
}
