package collector

import (
	"context"
	"testing"
	"time"

	coregit "github.com/grovetools/core/git"
	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
)

type fakeGitMirrorClient struct {
	workspaces []*models.EnrichedWorkspace
	stream     chan coredaemon.StateUpdate
}

func (f *fakeGitMirrorClient) GetEnrichedWorkspaces(context.Context, *models.EnrichmentOptions) ([]*models.EnrichedWorkspace, error) {
	return f.workspaces, nil
}

func (f *fakeGitMirrorClient) StreamState(context.Context, ...coredaemon.StreamFilter) (<-chan coredaemon.StateUpdate, error) {
	return f.stream, nil
}
func (f *fakeGitMirrorClient) Close() error { return nil }

func TestGlobalGitMirrorSnapshotsAndForwardsOnlyGitDeltas(t *testing.T) {
	path := "/repo"
	client := &fakeGitMirrorClient{
		workspaces: []*models.EnrichedWorkspace{{
			WorkspaceNode: &workspace.WorkspaceNode{Path: path},
			GitStatus:     &coregit.ExtendedGitStatus{StatusInfo: &coregit.StatusInfo{Branch: "main"}},
		}},
		stream: make(chan coredaemon.StateUpdate, 2),
	}
	updates := make(chan store.Update, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector := NewGlobalGitMirrorCollector()
	done := make(chan struct{})
	go func() {
		collector.runConnected(ctx, client, nil, updates)
		close(done)
	}()

	first := <-updates
	if first.Source != "git_mirror" || first.Scanned != 1 {
		t.Fatalf("snapshot update = %+v", first)
	}

	client.stream <- coredaemon.StateUpdate{
		UpdateType: "workspaces_delta", Source: "note",
		WorkspaceDeltas: []*models.WorkspaceDelta{{Path: path, GitStatus: &coregit.ExtendedGitStatus{}}},
	}
	client.stream <- coredaemon.StateUpdate{
		UpdateType: "workspaces_delta", Source: "git_watcher",
		WorkspaceDeltas: []*models.WorkspaceDelta{{Path: path, GitStatus: &coregit.ExtendedGitStatus{StatusInfo: &coregit.StatusInfo{Branch: "feature"}}}},
	}
	select {
	case update := <-updates:
		if update.Source != "git_mirror" || update.Scanned != 1 {
			t.Fatalf("stream update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("git delta was not mirrored")
	}
	select {
	case extra := <-updates:
		t.Fatalf("non-git delta was mirrored: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mirror did not stop on cancellation")
	}
}
