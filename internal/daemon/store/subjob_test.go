package store

import (
	"strings"
	"testing"
	"time"

	"github.com/grovetools/core/pkg/models"
)

func TestSubjobFoldIsMonotonicAndSnapshotIsDefensive(t *testing.T) {
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
}
