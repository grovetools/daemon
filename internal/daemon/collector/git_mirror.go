package collector

import (
	"context"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/paths"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const gitMirrorRetryInterval = 5 * time.Second

// gitMirrorClient is the connect-only global-daemon surface used by scoped
// daemons. Keeping the interface small makes the state-transfer rules testable.
type gitMirrorClient interface {
	GetEnrichedWorkspaces(context.Context, *models.EnrichmentOptions) ([]*models.EnrichedWorkspace, error)
	StreamState(context.Context) (<-chan coredaemon.StateUpdate, error)
	Close() error
}

// GlobalGitMirrorCollector makes scoped daemons pure consumers of global git
// state. It never starts the global daemon and never executes git itself.
type GlobalGitMirrorCollector struct {
	newClient func() (gitMirrorClient, error)
	retry     time.Duration
}

func NewGlobalGitMirrorCollector() *GlobalGitMirrorCollector {
	return &GlobalGitMirrorCollector{
		newClient: func() (gitMirrorClient, error) {
			return coredaemon.NewRemoteClient(paths.SocketPath(""))
		},
		retry: gitMirrorRetryInterval,
	}
}

func (c *GlobalGitMirrorCollector) Name() string { return "git_mirror" }

func (c *GlobalGitMirrorCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	localUpdates := st.Subscribe()
	defer st.Unsubscribe(localUpdates)
	for {
		if ctx.Err() != nil {
			return nil
		}
		client, err := c.newClient()
		if err != nil {
			if !sleepCtx(ctx, c.retry) {
				return nil
			}
			continue
		}
		if c.runConnected(ctx, client, localUpdates, updates) {
			_ = client.Close()
			return nil
		}
		_ = client.Close()
		if !sleepCtx(ctx, c.retry) {
			return nil
		}
	}
}

// runConnected returns true only for parent-context cancellation.
func (c *GlobalGitMirrorCollector) runConnected(ctx context.Context, client gitMirrorClient, localUpdates <-chan store.Update, updates chan<- store.Update) bool {
	if !c.snapshot(ctx, client, updates) {
		return ctx.Err() != nil
	}
	stream, err := client.StreamState(ctx)
	if err != nil {
		return ctx.Err() != nil
	}
	for {
		select {
		case <-ctx.Done():
			return true
		case local := <-localUpdates:
			// Workspace discovery may race the connect snapshot. Re-snapshot once
			// rows exist so early mirrored deltas cannot be dropped as unknown.
			if local.Type == store.UpdateWorkspaces && !c.snapshot(ctx, client, updates) {
				return ctx.Err() != nil
			}
		case event, ok := <-stream:
			if !ok {
				return false
			}
			if event.UpdateType == "initial" || event.UpdateType == "full" {
				if !c.snapshot(ctx, client, updates) {
					return ctx.Err() != nil
				}
				continue
			}
			if event.UpdateType != "workspaces_delta" || (event.Source != "git" && event.Source != "git_watcher") {
				continue
			}
			deltas := gitOnlyDeltas(event.WorkspaceDeltas)
			if len(deltas) > 0 {
				select {
				case updates <- store.Update{Type: store.UpdateWorkspacesDelta, Source: "git_mirror", Scanned: len(deltas), Payload: deltas}:
				case <-ctx.Done():
					return true
				}
			}
		}
	}
}

func (c *GlobalGitMirrorCollector) snapshot(ctx context.Context, client gitMirrorClient, updates chan<- store.Update) bool {
	workspaces, err := client.GetEnrichedWorkspaces(ctx, nil)
	if err != nil {
		return false
	}
	deltas := make([]*models.WorkspaceDelta, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws == nil || ws.WorkspaceNode == nil || ws.GitStatus == nil {
			continue
		}
		delta := &models.WorkspaceDelta{
			Path: ws.Path, GitStatus: ws.GitStatus, GitLanding: ws.GitLanding,
			ChangedFiles: ws.ChangedFiles, BlobHashes: ws.BlobHashes,
		}
		if ws.ChangedFilesComputed {
			computed := true
			delta.ChangedFilesComputed = &computed
		}
		deltas = append(deltas, delta)
	}
	if len(deltas) == 0 {
		return true
	}
	select {
	case updates <- store.Update{Type: store.UpdateWorkspacesDelta, Source: "git_mirror", Scanned: len(deltas), Payload: deltas}:
		return true
	case <-ctx.Done():
		return false
	}
}

func gitOnlyDeltas(in []*models.WorkspaceDelta) []*models.WorkspaceDelta {
	out := make([]*models.WorkspaceDelta, 0, len(in))
	for _, delta := range in {
		if delta != nil && delta.GitStatus != nil {
			out = append(out, delta)
		}
	}
	return out
}
