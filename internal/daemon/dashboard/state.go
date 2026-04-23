// Package dashboard aggregates cross-ecosystem grove env state into a single
// JSON payload for the browser dashboard and the (shrunk) TUI.
//
// The aggregator walks each configured grove source, reads every
// .grove/env/state.json it finds under recognised ecosystem roots, and
// assembles a view that merges on-disk state with the daemon's live service
// port/PID map. Both the HTTP handler (served on the dashboard TCP listener)
// and `grove env tui` consume this same shape.
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/config"
	coreenv "github.com/grovetools/core/pkg/env"
	"github.com/grovetools/core/pkg/workspace"
	daemonenv "github.com/grovetools/daemon/internal/daemon/env"
	"github.com/sirupsen/logrus"
)

// State is the top-level JSON payload returned by /api/dashboard/state.
type State struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Ecosystems  []Ecosystem `json:"ecosystems"`
	Errors      []string    `json:"errors,omitempty"`
}

// Ecosystem holds every worktree under a single grove-root.
type Ecosystem struct {
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Worktrees   []Worktree `json:"worktrees"`
	SharedInfra *SharedInfra `json:"shared_infra,omitempty"`
	Orphans     []Orphan    `json:"orphans"`
}

// Worktree summarises a single deployable worktree.
type Worktree struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Profile   string     `json:"profile,omitempty"`
	Provider  string     `json:"provider,omitempty"`
	State     string     `json:"state"`
	Endpoints []Endpoint `json:"endpoints"`
	Services  []Service  `json:"services"`
	Drift     *Drift     `json:"drift,omitempty"`
}

// Endpoint is a URL surfaced by the provider. `ok` reflects a short HEAD
// probe; when the probe is skipped or times out we leave it false.
type Endpoint struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	OK   bool   `json:"ok"`
}

// Service reports one declared service's runtime state.
type Service struct {
	Name   string `json:"name"`
	Port   int    `json:"port,omitempty"`
	PID    int    `json:"pid,omitempty"`
	Status string `json:"status"`
}

// Drift is a compact terraform-drift summary. Populated from the on-disk
// cache written by `grove env drift`.
type Drift struct {
	Profile  string    `json:"profile,omitempty"`
	HasDrift bool      `json:"has_drift"`
	Add      int       `json:"add"`
	Change   int       `json:"change"`
	Destroy  int       `json:"destroy"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// SharedInfra mirrors the per-ecosystem shared-profile drift block rendered
// alongside the worktree grid.
type SharedInfra struct {
	Profile  string `json:"profile"`
	Provider string `json:"provider,omitempty"`
	Drift    *Drift `json:"drift,omitempty"`
}

// Orphan is a state.json under .grove-worktrees/ with no live worktree.
type Orphan struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Worktree string `json:"worktree,omitempty"`
	Path     string `json:"path"`
}

// Aggregator builds a State snapshot. Holds a reference to the env manager
// so per-worktree service/port/PID info tracks live daemon state rather than
// just last-persisted state.json.
type Aggregator struct {
	Manager *daemonenv.Manager

	// HTTPClient is used for endpoint health probes. nil → skip probes.
	HTTPClient *http.Client
}

// New returns an Aggregator with sensible defaults: probes are enabled with
// a 500 ms timeout so one slow endpoint can't stall the dashboard poll.
func New(mgr *daemonenv.Manager) *Aggregator {
	return &Aggregator{
		Manager: mgr,
		HTTPClient: &http.Client{
			Timeout: 500 * time.Millisecond,
		},
	}
}

// Build walks cfg.Groves, resolves every discovered ecosystem root, and
// assembles a snapshot. Best-effort — individual errors are logged into
// State.Errors rather than aborting the whole payload.
func (a *Aggregator) Build(ctx context.Context, cfg *config.Config, probeEndpoints bool) State {
	out := State{GeneratedAt: time.Now().UTC()}
	if cfg == nil {
		return out
	}

	roots := ecosystemRoots(cfg)
	if len(roots) == 0 {
		return out
	}

	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.ErrorLevel)

	// workspace.GetProjects enumerates every known workspace from config —
	// we call it once and bucket nodes by ecosystem.
	allNodes, err := workspace.GetProjects(logger)
	if err != nil {
		out.Errors = append(out.Errors, "workspace discovery: "+err.Error())
	}

	for _, root := range roots {
		eco := Ecosystem{Name: root.Name, Path: root.Path, Worktrees: []Worktree{}, Orphans: []Orphan{}}

		// Collect nodes that belong to this ecosystem.
		var nodes []*workspace.WorkspaceNode
		for _, n := range allNodes {
			if n == nil {
				continue
			}
			if n.Kind != workspace.KindEcosystemRoot && n.Kind != workspace.KindEcosystemWorktree {
				continue
			}
			if belongsToEcosystem(n, root) {
				nodes = append(nodes, n)
			}
		}

		for _, n := range nodes {
			eco.Worktrees = append(eco.Worktrees, a.buildWorktree(ctx, n, probeEndpoints))
		}

		sort.Slice(eco.Worktrees, func(i, j int) bool { return eco.Worktrees[i].Name < eco.Worktrees[j].Name })

		eco.Orphans = detectOrphans(root.Path, nodes)
		eco.SharedInfra = sharedInfraFor(root.Path, ecoConfig(root.Path))

		out.Ecosystems = append(out.Ecosystems, eco)
	}

	sort.Slice(out.Ecosystems, func(i, j int) bool { return out.Ecosystems[i].Name < out.Ecosystems[j].Name })
	return out
}

// buildWorktree merges on-disk state with the live daemon port map. If the
// env manager tracks live state (env up ran this session) that wins for
// service status; the disk state fills profile/provider/endpoints/drift.
func (a *Aggregator) buildWorktree(ctx context.Context, node *workspace.WorkspaceNode, probe bool) Worktree {
	wt := Worktree{
		Name:      node.Name,
		Path:      node.Path,
		State:     "inactive",
		Endpoints: []Endpoint{},
		Services:  []Service{},
	}

	sf := readStateFile(filepath.Join(node.Path, ".grove", "env", "state.json"))
	if sf != nil {
		wt.Profile = sf.Environment
		wt.Provider = sf.Provider
		wt.State = "running"
		// Services: prefer disk state (has names + ports) and merge PIDs
		// from NativePGIDs when present so native runtimes show a PID.
		for _, svc := range sf.Services {
			s := Service{Name: svc.Name, Port: svc.Port, Status: svc.Status}
			if pgid, ok := sf.NativePGIDs[svc.Name]; ok {
				s.PID = pgid
			}
			wt.Services = append(wt.Services, s)
		}
		// Endpoint probe
		for _, url := range sf.Endpoints {
			ep := Endpoint{Name: endpointName(url), URL: url}
			if probe && a.HTTPClient != nil {
				ep.OK = a.probeEndpoint(ctx, url)
			}
			wt.Endpoints = append(wt.Endpoints, ep)
		}
	}

	// Layer live manager state on top (refreshes service status if still up).
	if a.Manager != nil {
		live := a.Manager.Status(node.Name)
		if live != nil && live.Status == "running" {
			wt.State = "running"
		}
	}

	return wt
}

func (a *Aggregator) probeEndpoint(ctx context.Context, url string) bool {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// WriteJSON is a helper used by the HTTP handler.
func WriteJSON(w http.ResponseWriter, s State) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s)
}

// probeConcurrent fires probes in parallel; kept for future use if the
// sequential path becomes a bottleneck.
func (a *Aggregator) probeConcurrent(ctx context.Context, urls []string) []bool {
	out := make([]bool, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			out[i] = a.probeEndpoint(ctx, u)
		}(i, u)
	}
	wg.Wait()
	return out
}

// ---- helpers ----

type ecosystemRoot struct {
	Name string
	Path string
}

func ecosystemRoots(cfg *config.Config) []ecosystemRoot {
	seen := map[string]bool{}
	var roots []ecosystemRoot
	for _, src := range cfg.Groves {
		if src.Enabled != nil && !*src.Enabled {
			continue
		}
		if src.Path == "" {
			continue
		}
		base := expandHome(src.Path)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			full := filepath.Join(base, e.Name())
			if !isEcosystemRoot(full) {
				continue
			}
			if seen[full] {
				continue
			}
			seen[full] = true
			roots = append(roots, ecosystemRoot{Name: e.Name(), Path: full})
		}
	}
	return roots
}

// isEcosystemRoot is a cheap check: the path contains a grove.toml (or
// .grove/) and a .grove-worktrees directory — the two signals every
// ecosystem root carries.
func isEcosystemRoot(path string) bool {
	if _, err := os.Stat(filepath.Join(path, ".grove-worktrees")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(path, "grove.toml")); err == nil {
		return true
	}
	return false
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func belongsToEcosystem(node *workspace.WorkspaceNode, root ecosystemRoot) bool {
	if pathsEqual(node.Path, root.Path) {
		return true
	}
	if node.ParentEcosystemPath != "" && pathsEqual(node.ParentEcosystemPath, root.Path) {
		return true
	}
	if node.RootEcosystemPath != "" && pathsEqual(node.RootEcosystemPath, root.Path) {
		return true
	}
	return false
}

func pathsEqual(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func readStateFile(path string) *coreenv.EnvStateFile {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var sf coreenv.EnvStateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil
	}
	return &sf
}

func endpointName(url string) string {
	s := strings.TrimPrefix(url, "https://")
	s = strings.TrimPrefix(s, "http://")
	if i := strings.Index(s, "."); i > 0 {
		return s[:i]
	}
	if i := strings.Index(s, "/"); i > 0 {
		return s[:i]
	}
	return s
}

func detectOrphans(ecosystemRoot string, worktrees []*workspace.WorkspaceNode) []Orphan {
	orphans := []Orphan{}
	pattern := filepath.Join(ecosystemRoot, ".grove-worktrees", "*", ".grove", "env", "state.json")
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return orphans
	}
	known := map[string]struct{}{}
	for _, w := range worktrees {
		known[strings.ToLower(filepath.Clean(w.Path))] = struct{}{}
	}
	sort.Strings(matches)
	for _, m := range matches {
		wtDir := filepath.Dir(filepath.Dir(filepath.Dir(m)))
		if _, ok := known[strings.ToLower(filepath.Clean(wtDir))]; ok {
			continue
		}
		orphans = append(orphans, Orphan{
			Category: "local.state_file",
			Name:     filepath.Base(wtDir),
			Worktree: filepath.Base(wtDir),
			Path:     m,
		})
	}
	return orphans
}

// ecoConfig best-effort loads grove.toml from the ecosystem root so we can
// surface its shared-infra profile to the dashboard. A missing or malformed
// file returns nil; the dashboard just renders the worktree grid without
// shared-infra context.
func ecoConfig(root string) *config.Config {
	cfg, err := config.LoadFrom(root)
	if err != nil {
		return nil
	}
	return cfg
}

func sharedInfraFor(root string, cfg *config.Config) *SharedInfra {
	if cfg == nil {
		return nil
	}
	var profile string
	for name := range cfg.Environments {
		if config.IsSharedProfile(cfg, name) {
			profile = name
			break
		}
	}
	if profile == "" {
		return nil
	}
	si := &SharedInfra{Profile: profile}
	if cfg.Environments != nil {
		if ec := cfg.Environments[profile]; ec != nil {
			si.Provider = ec.Provider
		}
	}
	return si
}
