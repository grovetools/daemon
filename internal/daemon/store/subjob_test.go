package store

import (
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestSubjobFoldIsMonotonicAndSnapshotIsDefensive(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	s := New()
	planKey, digest := strings.Repeat("a", 64), strings.Repeat("b", 64)
	ready := &models.SubjobEvent{SchemaVersion: 1, Kind: models.SubjobReportReady, PlanKey: planKey, ParentJobID: "parent", ChildJobID: "child", ReportSHA256: digest, Timestamp: time.Now().UTC()}
	s.ApplyUpdate(Update{Type: UpdateSubjobReportReady, Payload: ready})
	s.ApplyUpdate(Update{Type: UpdateSubjobJoined, Payload: &models.SubjobEvent{SchemaVersion: 1, Kind: models.SubjobJoined, PlanKey: planKey, ParentJobID: "parent", ChildJobID: "child", ReportSHA256: digest, Timestamp: time.Now().UTC()}})
	// Delayed ready cannot regress a joined tombstone.
	s.ApplyUpdate(Update{Type: UpdateSubjobReportReady, Payload: ready})
	snap := s.GetSubjobSnapshot(planKey, "parent")
	if got := snap.Reports["child"]; got == nil || got.State != models.SubjobJoined {
		t.Fatalf("state = %+v, want joined", got)
	}
	snap.Reports["child"].State = models.SubjobReportReady
	if got := s.GetSubjobSnapshot(planKey, "parent").Reports["child"].State; got != models.SubjobJoined {
		t.Fatalf("snapshot mutated store: %s", got)
	}
	if len(s.GetSubjobSnapshot(planKey, "other").Reports) != 0 {
		t.Fatal("snapshot leaked another parent's child")
	}

	// A canonically derived report with a new digest starts a new lifecycle.
	newDigest := strings.Repeat("c", 64)
	s.ApplyUpdate(Update{Type: UpdateSubjobReportReady, Payload: &models.SubjobEvent{
		SchemaVersion: 1, Kind: models.SubjobReportReady, PlanKey: planKey,
		ParentJobID: "parent", ChildJobID: "child", ReportSHA256: newDigest, Timestamp: time.Now().UTC(),
	}})
	if got := s.GetSubjobSnapshot(planKey, "parent").Reports["child"]; got.State != models.SubjobReportReady || got.ReportSHA256 != newDigest {
		t.Fatalf("new report generation not accepted: %+v", got)
	}
}

func TestSubjobStatePersistsAcrossStoreRecreation(t *testing.T) {
	t.Setenv("GROVE_HOME", t.TempDir())
	planKey, digest := strings.Repeat("d", 64), strings.Repeat("e", 64)
	s := New()
	s.ApplyUpdate(Update{Type: UpdateSubjobReportReady, Payload: &models.SubjobEvent{
		SchemaVersion: 1, Kind: models.SubjobReportReady, PlanKey: planKey,
		ParentJobID: "parent", ChildJobID: "child", ReportSHA256: digest, Timestamp: time.Now().UTC(),
	}})

	reloaded := New()
	got := reloaded.GetSubjobSnapshot(planKey, "parent").Reports["child"]
	if got == nil || got.State != models.SubjobReportReady || got.ReportSHA256 != digest {
		t.Fatalf("reloaded state = %+v", got)
	}
}
