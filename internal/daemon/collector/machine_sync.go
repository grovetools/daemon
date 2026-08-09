package collector

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/core/pkg/registry"
	"github.com/grovetools/daemon/internal/daemon/store"
)

const defaultMachineSyncInterval = time.Minute

// MachineSyncCollector projects the replicated machine registry onto each
// local workspace. It is tier 0 only: it reads registry notes and local git
// metadata, never a forge and never the network.
type MachineSyncCollector struct {
	interval time.Duration
	now      func() time.Time
}

func NewMachineSyncCollector(interval time.Duration) *MachineSyncCollector {
	if interval <= 0 {
		interval = defaultMachineSyncInterval
	}
	return &MachineSyncCollector{interval: interval, now: time.Now}
}

func (c *MachineSyncCollector) Name() string { return "machine_sync" }

func (c *MachineSyncCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	retry := time.NewTimer(time.Second)
	defer retry.Stop()

	scan := func() bool {
		state := st.Get()
		if len(state.Workspaces) == 0 {
			return false // workspace discovery has not seeded the store yet
		}

		projections := c.loadAndProject(state.Workspaces)
		deltas := make([]*models.WorkspaceDelta, 0, len(projections))
		for path, projection := range projections {
			if current := state.Workspaces[path]; current != nil && reflect.DeepEqual(current.MachineSync, projection) {
				continue
			}
			deltas = append(deltas, &models.WorkspaceDelta{Path: path, MachineSync: projection})
		}
		sort.Slice(deltas, func(i, j int) bool { return deltas[i].Path < deltas[j].Path })
		if len(deltas) > 0 {
			updates <- store.Update{Type: store.UpdateWorkspacesDelta, Source: "machine_sync", Payload: deltas}
		}
		return true
	}

	if scan() && !retry.Stop() {
		<-retry.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-retry.C:
			if !scan() {
				retry.Reset(time.Second)
			}
		case <-ticker.C:
			scan()
		}
	}
}

func (c *MachineSyncCollector) loadAndProject(workspaces map[string]*models.EnrichedWorkspace) map[string]*models.MachineSync {
	localID := machine.ID()
	machineCfg, machineErr := config.LoadMachineConfig()
	if machineErr != nil {
		return unavailableMachineSync(workspaces, localID, "machine config unavailable")
	}
	cfg, cfgErr := config.LoadDefault()
	if cfgErr != nil {
		return unavailableMachineSync(workspaces, localID, "grove config unavailable")
	}
	syncCfg, syncErr := config.LoadSyncConfig()
	if syncErr != nil {
		return unavailableMachineSync(workspaces, localID, "sync config unavailable")
	}
	sub := registry.Subscription(syncCfg)
	if sub == nil {
		return unavailableMachineSync(workspaces, localID, "registry subscription unavailable")
	}
	root := registry.WorkspaceRoot(cfg, sub.Name)
	machines, err := registry.ReadMachines(root, localID)
	if err != nil {
		return unavailableMachineSync(workspaces, localID, "registry replica unavailable")
	}
	return projectMachineSync(workspaces, localID, machineCfg, machines, c.now())
}

func unavailableMachineSync(workspaces map[string]*models.EnrichedWorkspace, localID, reason string) map[string]*models.MachineSync {
	out := make(map[string]*models.MachineSync, len(workspaces))
	for path := range workspaces {
		out[path] = &models.MachineSync{
			SchemaVersion:  models.MachineSyncSchemaVersion,
			LocalMachineID: localID,
			Peers:          []models.MachineSyncPeer{},
			Error:          reason,
		}
	}
	return out
}

type machineSyncRoot struct {
	name      string
	id        string
	path      string
	ecosystem bool
}

func projectMachineSync(workspaces map[string]*models.EnrichedWorkspace, localID string, machineCfg *config.MachineConfig, machines []registry.Machine, now time.Time) map[string]*models.MachineSync {
	roots := localMachineSyncRoots(machineCfg)
	out := make(map[string]*models.MachineSync, len(workspaces))
	for path, ws := range workspaces {
		identityPath := path
		if ws != nil && ws.ParentProjectPath != "" {
			identityPath = ws.ParentProjectPath
		}
		root, repoPath, identified := identifyMachineSyncRepo(identityPath, roots)
		branch, sha, tipKnown := registry.ReadRepoTip(path)
		projection := &models.MachineSync{
			SchemaVersion:  models.MachineSyncSchemaVersion,
			LocalMachineID: localID,
			LocalBranch:    branch,
			LocalSHA:       sha,
			Peers:          []models.MachineSyncPeer{},
		}
		if identified {
			projection.RootID = root.id
			projection.RepoPath = repoPath
		}
		for _, remote := range machines {
			if remote.Self || remote.PathID == localID {
				continue
			}
			peer := projectMachineSyncPeer(remote, root, repoPath, identified, branch, sha, tipKnown, now)
			projection.Peers = append(projection.Peers, peer)
		}
		out[path] = projection
	}
	return out
}

func localMachineSyncRoots(cfg *config.MachineConfig) []machineSyncRoot {
	if cfg == nil {
		return nil
	}
	var roots []machineSyncRoot
	for _, state := range config.ReconcileMachineEcosystems(cfg) {
		id := "ecosystem-name:" + state.Name
		if state.Manifest != "" {
			if card, err := config.LoadEcosystemCard(state.Manifest); err == nil && card != nil && card.ID != "" {
				id = "ecosystem:" + card.ID
			}
		}
		roots = append(roots, machineSyncRoot{name: state.Name, id: id, path: filepath.Clean(state.Path), ecosystem: true})
	}
	for name, root := range cfg.Machine.Roots {
		roots = append(roots, machineSyncRoot{name: name, id: "root:" + name, path: expandMachineSyncPath(root.Path)})
	}
	// Longest path first prevents a nested configured root from being claimed
	// by its parent.
	sort.Slice(roots, func(i, j int) bool { return len(roots[i].path) > len(roots[j].path) })
	return roots
}

func expandMachineSyncPath(path string) string {
	path = os.ExpandEnv(path)
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func identifyMachineSyncRepo(path string, roots []machineSyncRoot) (machineSyncRoot, string, bool) {
	path = filepath.Clean(path)
	for _, root := range roots {
		rel, err := filepath.Rel(root.path, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if rel == "." {
			return root, ".", true
		}
		// Registry tip collection is deliberately bounded to immediate children.
		if !strings.Contains(rel, string(filepath.Separator)) {
			return root, filepath.ToSlash(rel), true
		}
	}
	return machineSyncRoot{}, "", false
}

func projectMachineSyncPeer(remote registry.Machine, localRoot machineSyncRoot, repoPath string, identified bool, localBranch, localSHA string, localTipKnown bool, now time.Time) models.MachineSyncPeer {
	peer := models.MachineSyncPeer{
		MachineID: remote.PathID,
		Label:     remote.Label(),
		State:     models.MachineSyncUnknown,
		Suspect:   remote.Suspicious() || remote.Err != nil,
	}
	if remote.Note != nil {
		peer.LastSeen = remote.Note.LastSeen
	}
	if age, ok := remote.StaleFor(now); ok {
		seconds := int64(age / time.Second)
		peer.AgeSeconds = &seconds
	}
	if peer.Suspect || remote.Note == nil {
		peer.Reason = "registry note is suspect or unreadable"
		return peer
	}
	if !identified {
		peer.Reason = "local workspace has no configured root identity"
		return peer
	}

	remoteRoot, present := findRemoteMachineSyncRoot(remote.Note, localRoot)
	if !present {
		peer.Reason = "machine does not declare this root"
		return peer
	}
	if !remoteRoot.enabled || !remoteRoot.includes(repoPath) {
		peer.State = models.MachineSyncExcluded
		return peer
	}
	if !remoteRoot.present {
		peer.State = models.MachineSyncAbsent
		return peer
	}

	var tip *registry.NoteRepo
	for i := range remote.Note.Repos {
		candidate := &remote.Note.Repos[i]
		if candidate.Root == remoteRoot.name && filepath.ToSlash(candidate.Path) == repoPath {
			tip = candidate
			break
		}
	}
	if tip == nil {
		peer.State = models.MachineSyncAbsent
		return peer
	}
	peer.Branch, peer.SHA = tip.Branch, tip.SHA
	peer.SameBranch = localBranch == tip.Branch
	if !localTipKnown || localSHA == "" || tip.SHA == "" {
		peer.Reason = "one or both committed tips are unavailable"
		return peer
	}
	if localSHA == tip.SHA {
		peer.State = models.MachineSyncEqual
	} else {
		peer.State = models.MachineSyncDiverged
		peer.Reason = "committed tips differ; direction and distance are unknown"
	}
	return peer
}

type remoteMachineSyncRoot struct {
	name    string
	enabled bool
	present bool
	repos   []string
	exclude []string
}

func (r remoteMachineSyncRoot) includes(repoPath string) bool {
	if repoPath == "." {
		return true // partial superrepo subscriptions still materialize the root
	}
	if len(r.repos) > 0 {
		return containsMachineSyncString(r.repos, repoPath)
	}
	return !containsMachineSyncString(r.exclude, repoPath)
}

func containsMachineSyncString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func findRemoteMachineSyncRoot(note *registry.Note, local machineSyncRoot) (remoteMachineSyncRoot, bool) {
	if local.ecosystem {
		localCardID := strings.TrimPrefix(local.id, "ecosystem:")
		for _, eco := range note.Ecosystems {
			idMatches := strings.HasPrefix(local.id, "ecosystem:") && eco.Card != nil && eco.Card.ID == localCardID
			if !idMatches && eco.Name != local.name {
				continue
			}
			return remoteMachineSyncRoot{
				name: eco.Name, enabled: eco.Enabled,
				present: eco.State == registry.StatePresent || eco.State == registry.StateUnmanifested,
				repos:   eco.Repos, exclude: eco.Exclude,
			}, true
		}
		return remoteMachineSyncRoot{}, false
	}
	for _, root := range note.Roots {
		if root.Name == local.name {
			return remoteMachineSyncRoot{name: root.Name, enabled: root.Enabled, present: root.Exists}, true
		}
	}
	return remoteMachineSyncRoot{}, false
}
