package watcher

// Pipeline lifecycle (Phase 3, W3.3 + W3.6).
//
// Before this file, ensurePipelines was add-only: it walked the desired
// notespaces, registered each one, and started a transport for any that had no
// entry in the pipelines map. Nothing ever removed one. The consequences were
// all of a kind — the daemon's running transports were a record of everything
// that had EVER been configured, not of what was configured now:
//
//   - Removing a subscription from sync.toml stopped nothing. The pipeline kept
//     pushing and (for pull = true) kept writing into the notebook until the
//     daemon was restarted. The only teardown in the tree was the auth reset
//     (sync_auth.go's resetTransport), which drops everything wholesale.
//   - Re-pointing a notebook at a new root did not move the transport. Push,
//     pull and anti-entropy each captured their root as a call argument at
//     spawn time, so the pipeline went on syncing the OLD directory: the new
//     root stayed dark and the old one kept replicating.
//
// The replacement is a reconcile: compute the desired set, stop what is no
// longer wanted or is wanted at a different root, then start what is missing.
// Three properties make it safe to run from three different goroutines:
//
//	Serialized. reconcileMu is held for the whole pass. Registration does
//	network I/O inside it, so passes queue rather than interleave; the
//	callers (transport tick, watch refresh, adoption notification) were
//	already paying that latency before this change.
//
//	Generation-stamped. Every config reload bumps configGeneration. A pass
//	reads it once, stamps it onto everything it installs, and abandons the
//	rest of its work the moment it observes a newer one — a pipeline is never
//	started against config that has already been superseded.
//
//	Drain-before-respawn. Cancelling a pipeline does not make its goroutines
//	stop instantly (a pull sits in a 30s long-poll). A re-rooted notespace
//	therefore does not start its replacement in the same pass: the old
//	pipeline moves to `draining` and the new one starts on the first pass
//	that observes every one of its goroutines has returned. That is what
//	stops two transports for one notespace id from writing into two roots.
//
// Duplicate stamps (D8 / W3.6) are resolved here too, because "which root owns
// this id" has to be answered before anything registers or spawns: the
// first-seen root keeps syncing, later roots are parked with evidence naming
// both paths, and parking is idempotent — the evidence is written once per
// episode, not once per ten-second tick.

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/machine"
	notespacepkg "github.com/grovetools/core/pkg/notespace"
	"github.com/grovetools/core/pkg/workspace"
	"github.com/grovetools/daemon/internal/daemon/store"
	syncdb "github.com/grovetools/daemon/internal/daemon/sync"
)

// defaultDuplicateScanInterval bounds the containing-notebook sweep that finds
// duplicate stamps in roots nothing is subscribed to (the `cp -R` case). It is
// read-only — a readdir plus a stamp load per sibling — but there is no reason
// to repeat it on every ten-second transport tick.
const defaultDuplicateScanInterval = time.Minute

// drainWaitWarnPasses is how many consecutive reconcile passes may find the same
// pipeline still draining before the wait stops being routine. A pull sits in
// a 30s long-poll and the transport ticks every 10s, so a healthy re-root is
// expected to block a handful of passes; past that the goroutine is wedged and
// the notespace has no transport.
const drainWaitWarnPasses = 6

// pipelineState is one running per-notespace transport.
type pipelineState struct {
	cancel context.CancelFunc
	// done is closed once every goroutine of this pipeline has returned. It is
	// the drain signal a re-root waits on.
	done <-chan struct{}
	// root is the directory the pipeline was STARTED against. Push, pull and
	// anti-entropy all captured it, so a config change that moves the root
	// makes this pipeline wrong rather than stale.
	root string
	pull bool
	// push is the push/anti-entropy side of the same desired state. Both are
	// recorded so the health test in stopUndesired can compare a running
	// pipeline against what the contested set says NOW, rather than treating
	// the verdict at birth as permanent.
	push bool
	// generation is the config generation that installed this pipeline.
	generation uint64
	// drainWaits counts reconcile passes that found this cancelled pipeline
	// still draining. Touched only under pipelinesMu.
	drainWaits int
}

// stopped reports whether every goroutine of a cancelled pipeline has returned.
func (p *pipelineState) stopped() bool {
	if p == nil || p.done == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// ParkedNotespace is one notespace this daemon refuses to sync, and why. It is
// the D8 verdict in structured form, for status/doctor surfaces and for the
// tests that assert parking without reading log lines.
type ParkedNotespace struct {
	// NotespaceID is the immutable stamp id both roots claim.
	NotespaceID string `json:"notespace_id"`
	// Name is the parked root's display name (evidence only, never a key).
	Name string `json:"notespace_name,omitempty"`
	// Root is the parked directory: the copy that does NOT sync.
	Root string `json:"root"`
	// Keeper is the first-seen root that keeps syncing.
	Keeper string `json:"keeper_root"`
	// Reason is a conflict kind (syncdb.ConflictKind*).
	Reason string `json:"reason"`
	// Detail is the operator-facing sentence written to the conflicts feed.
	Detail string `json:"detail"`
}

// bumpConfigGeneration records that the recorded config changed under a
// running daemon. Called by the config-reload path; the value is stamped onto
// pipelines and re-read mid-pass so a reconcile cannot install stale routing.
func (h *SyncHandler) bumpConfigGeneration() uint64 {
	return h.configGeneration.Add(1)
}

// resolvedNotespace is one desired subscription after its identity has been
// read from disk and duplicate stamps have been resolved.
type resolvedNotespace struct {
	id          string
	displayName string
	root        string
	stamp       *notespacepkg.NotespaceStamp
	sub         *config.SyncWorkspace
}

// ensurePipelines reconciles running transports against recorded config. It is
// idempotent and safe to call from any goroutine.
func (h *SyncHandler) ensurePipelines() {
	h.reconcileMu.Lock()
	defer h.reconcileMu.Unlock()

	h.clientMu.RLock()
	client := h.client
	h.clientMu.RUnlock()
	// A client only exists once the DB is open (transportLoop connects after
	// ensureDB), so db is non-nil here — the check keeps that ordering explicit
	// rather than assumed.
	db := h.database()
	if client == nil || db == nil || h.baseCtx == nil {
		return
	}

	generation := h.configGeneration.Load()

	desired, err := h.desiredRoots()
	if err != nil {
		h.ulog.Error("sync routing configuration error; pipelines not started").Err(err).Log(h.baseCtx)
		return
	}

	resolved := h.resolveIdentities(desired)

	h.reclaimDrained()
	h.stopUndesired(resolved, desired, generation)

	ids := slices.Sorted(maps.Keys(resolved))
	for _, id := range ids {
		// The recorded config changed while this pass was registering. Whatever
		// is left of the desired set was computed from superseded truth; the
		// reconcile the reload triggers has the current one.
		if h.configGeneration.Load() != generation {
			h.ulog.Debug("sync reconcile abandoned: config generation advanced").
				Field("generation", generation).
				StructuredOnly().Log(h.baseCtx)
			return
		}
		h.startPipeline(client, db, resolved[id], generation)
	}

	// Registration gave every notespace above an identity on the server; this
	// gives the contained ones the notebook they belong to. It runs after the
	// loop, not inside startPipeline, for two reasons: an attach is refused
	// before the registration it depends on has landed, and a notespace whose
	// pipeline was already running from an earlier pass — a notebook shared
	// after the fact, a machine that predates this rule — would otherwise never
	// be asked about at all.
	h.attachContainedNotespaces(client, resolved)

	// Capture switches to immutable identity only for roots that have a
	// REGISTERED, running transport — including after a watch-set refresh that
	// rebuilt the watch entries with an empty notespace field. Binding earlier
	// would let a display name or an unregistered root become a durable DB/wire
	// key, and would unpark a duplicate by the back door.
	h.bindWatchIdentities()
}

// desiredRoots is display name -> notespace root for everything recorded
// config says this machine syncs. Two sources, exactly as before: the
// discovery-driven watch set (push-side real trees), and direct config
// resolution for pull = true subscriptions, so a pull replica is desired even
// when code discovery finds nothing.
func (h *SyncHandler) desiredRoots() (map[string]string, error) {
	roots := make(map[string]string)
	h.pathsMutex.RLock()
	for _, w := range h.watchedPaths {
		name := w.displayName
		if name == "" { // direct test/legacy construction
			name = w.notespace
		}
		if name != "" {
			roots[name] = w.root
		}
	}
	h.pathsMutex.RUnlock()

	pullRoots, err := h.configuredPullRoots()
	if err != nil {
		return nil, err
	}
	for name, root := range pullRoots {
		if _, ok := roots[name]; !ok {
			roots[name] = root
		}
	}
	return roots, nil
}

// resolveIdentities reads each desired root's stamp and returns the notespaces
// that may sync, keyed by immutable id. Roots whose stamp cannot be read are
// reported and left out (their existing pipeline, if any, is preserved by
// stopUndesired — an unreadable stamp is usually transient and must not tear a
// working transport down). Duplicate ids are settled here: exactly one root per
// id survives, the rest are parked.
func (h *SyncHandler) resolveIdentities(desired map[string]string) map[string]resolvedNotespace {
	// Deterministic order: by root, as the pre-P3 pass did.
	names := slices.Collect(maps.Keys(desired))
	slices.SortFunc(names, func(a, b string) int { return strings.Compare(desired[a], desired[b]) })

	candidates := make(map[string][]resolvedNotespace)
	for _, displayName := range names {
		root := desired[displayName]
		stamp, err := notespacepkg.LoadNotespace(root)
		if err != nil || stamp == nil {
			if err == nil {
				err = fmt.Errorf("notespace %q at %s has no .notespace.toml; run grove migrate (step 2)", displayName, root)
			} else {
				err = fmt.Errorf("load notespace identity at %s: %w", root, err)
			}
			h.ulog.Error("notespace registration failed; pipeline parked").Err(err).Field("root", root).Log(h.baseCtx)
			continue
		}
		sub := h.effectiveSubscription(displayName, root)
		candidates[stamp.ID] = append(candidates[stamp.ID], resolvedNotespace{
			id: stamp.ID, displayName: displayName, root: root, stamp: stamp, sub: sub,
		})
	}

	// Widen the duplicate search beyond the desired set: a `cp -R` of a
	// notespace inside its own notebook produces a second root carrying the
	// same stamp that nothing is subscribed to, so it would never appear above
	// and the copy would sit there looking synced. The sweep only DETECTS —
	// it never promotes a root into the desired set.
	siblings := h.duplicateSiblings(candidates)

	resolved := make(map[string]resolvedNotespace, len(candidates))
	parked := make(map[string]ParkedNotespace)
	for id, group := range candidates {
		keeper := h.firstSeenRoot(id, group, siblings[id])
		// The keeper can be a swept sibling rather than one of the desired
		// candidates — the durable binding names the root this machine has
		// been syncing, and nothing says that root is still subscribed. The
		// sweep DETECTS but never promotes, so the honest outcome is that this
		// id resolves to nobody: every desired copy parks, no pipeline runs,
		// and the operator re-mints. Letting an unbound copy inherit the
		// identity instead is precisely the D8 inversion this answers.
		keeperDesired := false
		for _, cand := range group {
			if samePhysicalPath(cand.root, keeper) {
				keeperDesired = true
				break
			}
		}
		for _, cand := range group {
			if samePhysicalPath(cand.root, keeper) {
				resolved[id] = cand
				continue
			}
			detail := fmt.Sprintf("duplicate notespace id %s: first-seen root %s keeps syncing; %s is parked "+
				"(no push, no pull) until one of the two is re-minted — `grove doctor --fix` re-mints the copy you designate",
				id, keeper, cand.root)
			if !keeperDesired {
				detail = fmt.Sprintf("duplicate notespace id %s: this machine's recorded sync history belongs to %s, "+
					"which is not subscribed, so %s is parked (no push, no pull) rather than taking over the id — "+
					"re-mint one of the two (`grove doctor --fix`) or subscribe the recorded root",
					id, keeper, cand.root)
			}
			parked[parkKey(id, cand.root)] = ParkedNotespace{
				NotespaceID: id,
				Name:        cand.stamp.Name,
				Root:        cand.root,
				Keeper:      keeper,
				Reason:      syncdb.ConflictKindDuplicateStamp,
				Detail:      detail,
			}
		}
		for _, sibling := range siblings[id] {
			if samePhysicalPath(sibling, keeper) {
				continue
			}
			parked[parkKey(id, sibling)] = ParkedNotespace{
				NotespaceID: id,
				Name:        group[0].stamp.Name,
				Root:        sibling,
				Keeper:      keeper,
				Reason:      syncdb.ConflictKindDuplicateStamp,
				Detail: fmt.Sprintf("duplicate notespace id %s: first-seen root %s keeps syncing; the copy at %s is parked "+
					"(it is not synced under any identity) until it is re-minted — `grove doctor --fix` re-mints the copy you designate",
					id, keeper, sibling),
			}
		}
	}

	h.recordParked(parked)

	// A contested notespace (the W3.5 adoption seam) still RESOLVES — it keeps
	// its identity, its root binding and its watch — but startPipeline gives it
	// neither loop, so nothing is written into it and nothing leaves it until
	// it is adopted.
	return resolved
}

// firstSeenRoot answers D8's "first-seen keeps syncing" durably rather than by
// whichever root happened to sort first this pass:
//
//  1. the root sync.db already bound to this id — the only answer that
//     survives a daemon restart, and the one that keeps the machine's history
//     attached to the copy that has been syncing all along;
//  2. the root a pipeline is currently running against;
//  3. the lexicographically smallest root, so a first-ever observation of two
//     copies is at least deterministic.
//
// Two things the earlier shape got wrong, both of which INVERTED rung 1 in
// exactly the case it exists for:
//
//   - the candidate set was built only from `group`, the DESIRED subscriptions,
//     so a root the sweep found could never win — even when sync.db says it is
//     the root this machine has been syncing all along. Siblings are folded in
//     here (detection only: winning the vote does not put a sibling into the
//     desired set — see resolveIdentities, where a sibling keeper means NOBODY
//     syncs this id until the operator re-mints);
//   - a `len(roots) == 1` short-circuit ran BEFORE the binding lookup, so a
//     lone newly-subscribed copy took over an id the DB had bound to a sibling
//     without the durable answer ever being consulted.
func (h *SyncHandler) firstSeenRoot(id string, group []resolvedNotespace, siblings []string) string {
	roots := make([]string, 0, len(group)+len(siblings))
	for _, cand := range group {
		roots = append(roots, cand.root)
	}
	roots = append(roots, siblings...)
	sort.Strings(roots)
	roots = slices.Compact(roots)
	if len(roots) == 0 {
		return ""
	}

	if db := h.database(); db != nil {
		if binding, err := db.GetNotespaceBinding(id); err == nil && binding != nil {
			for _, root := range roots {
				if samePhysicalPath(root, binding.Root) {
					return root
				}
			}
		}
	}

	h.pipelinesMu.Lock()
	running := h.pipelines[id]
	h.pipelinesMu.Unlock()
	if running != nil {
		for _, root := range roots {
			if samePhysicalPath(root, running.root) {
				return root
			}
		}
	}
	return roots[0]
}

// duplicateSiblings sweeps the notespaces/ directory of every notebook that
// hosts a desired notespace and reports any OTHER stamped directory carrying
// an id the desired set already claims. Read-only and rate-limited; it exists
// so the `cp -R` case is detected rather than silently ignored.
//
// The rate limit is on the SWEEP, never on the verdict. Returning nil on a
// rate-limited pass made every sibling-derived parking decision vanish for the
// ~5 of every 6 transport ticks that skip the scan (10s tick, 60s sweep), with
// two consequences: recordParked saw the verdict as absent from `previous` on
// the next sweep and re-emitted duplicate_stamp evidence as a new episode once
// a minute forever, and isParkedRoot — the exclusion that keeps a parked copy
// out of the escrow apply and the maintenance drain — went false in between.
// The last sweep's result is therefore cached and replayed until the next one
// supersedes it. A copy that has since been re-minted stays parked for up to
// one scan interval, which is the conservative direction.
//
// A replay is re-checked against the disk first (liveSiblings), which is NOT
// the same conservatism read the other way. A cached sibling that has VANISHED
// is not a duplicate of anything: the P3 gate found a notebook root move being
// read as a duplicate stamp against the path it had just left, because the
// cache still named the old root, the durable binding still named it too, and
// a path that no longer exists therefore out-voted the live one — parking the
// relocated notespace and leaving a duplicate_stamp artifact naming a
// directory the operator cannot go look at. Re-checking only the cached roots
// costs a stamp read each, not the sweep the rate limit exists to bound.
func (h *SyncHandler) duplicateSiblings(candidates map[string][]resolvedNotespace) map[string][]string {
	interval := h.duplicateScanInterval
	if interval <= 0 {
		interval = defaultDuplicateScanInterval
	}
	if !h.duplicateScannedAt.IsZero() && time.Since(h.duplicateScannedAt) < interval {
		h.duplicateSiblingsCache = liveSiblings(h.duplicateSiblingsCache)
		return claimedSiblings(h.duplicateSiblingsCache, candidates)
	}
	h.duplicateScannedAt = time.Now()

	claimed := make(map[string]bool, len(candidates))
	notespaceDirs := make(map[string]bool)
	for id, group := range candidates {
		claimed[id] = true
		for _, cand := range group {
			parent := filepath.Dir(cand.root)
			if filepath.Base(parent) == workspace.NotespaceDirectory {
				notespaceDirs[parent] = true
			}
		}
	}

	found := make(map[string][]string)
	for dir := range notespaceDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			root := filepath.Join(dir, entry.Name())
			stamp, err := notespacepkg.LoadNotespace(root)
			if err != nil || stamp == nil || !claimed[stamp.ID] {
				continue
			}
			found[stamp.ID] = append(found[stamp.ID], root)
		}
	}
	for id := range found {
		sort.Strings(found[id])
	}
	h.duplicateSiblingsCache = found
	return claimedSiblings(found, candidates)
}

// claimedSiblings narrows a sweep result to the ids this pass actually claims.
// The cache outlives the candidate set that produced it, and a replayed verdict
// must not park a root for a notespace nothing is subscribed to any more.
func claimedSiblings(found map[string][]string, candidates map[string][]resolvedNotespace) map[string][]string {
	if len(found) == 0 {
		return nil
	}
	out := make(map[string][]string, len(found))
	for id, roots := range found {
		if len(candidates[id]) == 0 {
			continue
		}
		out[id] = roots
	}
	return out
}

// liveSiblings drops cached sibling roots that no longer carry the id the
// sweep recorded there.
//
// The check is the stamp, not the directory: a root that was moved away is
// gone, and a root whose stamp was re-minted (the `grove doctor --fix --remint`
// repair) no longer claims the id either. Both mean the same thing to the
// duplicate rule — there is nothing at that path contesting this identity — and
// asserting it against the stamp rather than against os.Stat is what keeps a
// re-minted copy from being replayed as a duplicate for the rest of the scan
// interval.
func liveSiblings(found map[string][]string) map[string][]string {
	if len(found) == 0 {
		return found
	}
	out := make(map[string][]string, len(found))
	for id, roots := range found {
		live := make([]string, 0, len(roots))
		for _, root := range roots {
			stamp, err := notespacepkg.LoadNotespace(root)
			if err != nil || stamp == nil || stamp.ID != id {
				continue
			}
			live = append(live, root)
		}
		if len(live) > 0 {
			out[id] = live
		}
	}
	return out
}

func parkKey(id, root string) string { return id + "\x00" + root }

// recordParked installs this pass's parking verdict and emits evidence for
// newly parked roots only. Rebuilding the map each pass is what makes parking
// self-clearing: a re-minted copy simply stops appearing.
func (h *SyncHandler) recordParked(current map[string]ParkedNotespace) {
	h.parkMu.Lock()
	previous := h.parked
	h.parked = current
	h.parkMu.Unlock()

	keys := slices.Sorted(maps.Keys(current))
	for _, key := range keys {
		if _, seen := previous[key]; seen {
			continue // same episode; evidence is already on the feed
		}
		entry := current[key]
		h.ulog.Error("notespace parked: duplicate stamp id").
			Field("notespace_id", entry.NotespaceID).
			Field("parked_root", entry.Root).
			Field("keeper_root", entry.Keeper).
			Log(h.baseCtx)
		if _, err := syncdb.WriteNotespaceConflict(entry.NotespaceID, entry.Reason, entry.Detail); err != nil {
			h.ulog.Warn("failed to record duplicate-stamp evidence").
				Field("notespace_id", entry.NotespaceID).Err(err).Log(h.baseCtx)
		}
		h.broadcastConflict(&store.SyncConflictPayload{
			Kind:          entry.Reason,
			NotespaceID:   entry.NotespaceID,
			NotespaceName: entry.Name,
			Path:          ".notespace.toml",
			Detail:        entry.Detail,
		})
	}
}

// ParkedNotespaces returns the current parking verdict, sorted by id then root.
//
// The operator-facing surface for a parking decision is the conflicts feed —
// recordParked writes an artifact-backed, restart-safe entry that
// GET /api/sync/conflicts already serves, so doctor and the TUI see duplicates
// without any new wiring. This accessor is the in-process view of the same
// verdict, for status adapters that want it structured and for tests.
func (h *SyncHandler) ParkedNotespaces() []ParkedNotespace {
	h.parkMu.Lock()
	defer h.parkMu.Unlock()
	out := make([]ParkedNotespace, 0, len(h.parked))
	for _, entry := range h.parked {
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b ParkedNotespace) int {
		if c := strings.Compare(a.NotespaceID, b.NotespaceID); c != 0 {
			return c
		}
		return strings.Compare(a.Root, b.Root)
	})
	return out
}

// ContestedNotespace is one notespace withheld by the W3.5 adoption gate, and
// the evidence the operator decides from — GET /api/sync/contested serves it
// and `grove sync adopt-notespace` renders it. It is defined beside the gate that
// produces it (syncdb) because the HTTP layer reads it too, and aliased here so
// the watcher's own surfaces read in the watcher's vocabulary.
type ContestedNotespace = syncdb.ContestedNotespace

// MarkContested is the W3.5 adoption seam.
//
// W3.5's rule is "no unadopted content moves in either direction": pulling a
// shared notebook onto pre-existing un-synced local notes must let clean
// documents flow while colliding subtrees wait for an operator decision. This
// is the ENFORCEMENT half:
//
//	MarkContested(id, reason) → the notespace loses BOTH pipelines. Nothing
//	incoming is written into the contested tree, and nothing outgoing leaves
//	it: no push loop, no anti-entropy sweep, and no maintenance drain. The
//	outbox is parked, not discarded — local edits keep queuing behind the
//	gate. ClearContested(id) is adoption: the next reconcile pass restores
//	both loops, and the queued local work goes out then.
//
// Withholding push is not symmetry for its own sake. The collision the gate
// detects has two machines in it, and an inbound-only gate protects the one
// that raised it while deciding the other side automatically in favour of the
// newcomer — see pushDesired for the four-step walk-through.
//
// The DETECTION half is the pull pipeline's pre-apply gate (sync/adoption.go),
// wired in through markContestedFromPull below: it withholds the batch, writes
// the evidence to the conflicts feed, and calls here. Nothing else marks a
// notespace contested — a verdict with no evidence behind it would be a
// notespace that stops syncing for reasons the operator cannot read.
func (h *SyncHandler) MarkContested(notespaceID, reason string) {
	h.markContested(ContestedNotespace{NotespaceID: notespaceID, Reason: reason})
}

// markContested installs a full verdict. MarkContested is the reason-only
// spelling; both go through here so the map has one writer.
//
// Installing the verdict is SYNCHRONOUSLY enough to stop this notespace's own
// outbound loops, and that is the whole point of where the write sits. The
// caller is usually the pull gate's OnContested, running on the pull goroutine
// of the pipeline that is also pushing; the loops read the same map through
// outboundVeto, so by the time this returns the next outbox batch — and the
// next anti-entropy phase — is already refused. What the reconcile pass adds
// afterwards is teardown, not enforcement. Before the veto was consulted by
// the loops, the reconcile WAS the enforcement, and the nine to twenty seconds
// it took to arrive were long enough for the push loop to drain the disputed
// tree to the server.
func (h *SyncHandler) markContested(entry ContestedNotespace) {
	if entry.NotespaceID == "" {
		return
	}
	h.parkMu.Lock()
	h.contested[entry.NotespaceID] = entry
	h.parkMu.Unlock()
}

// outboundVeto is the W3.5 gate as the RUNNING loops consult it: a live read of
// the contested set for one notespace id, handed to the push loop and the
// anti-entropy sweep at spawn time and evaluated per outbox batch and per pass.
//
// It is keyed by id and closes over nothing else on purpose. The alternative
// shape — a flag on pipelineState, flipped by OnContested — has a window that
// this one does not: startPipeline spawns push, anti-entropy and pull BEFORE it
// installs the pipelineState, so a contest raised by the pull loop's first
// batch can land while there is still no state to flip, and the loops that were
// already running would consult a flag nobody could reach. A notespace id
// exists from the first line of startPipeline, and the verdict is stored
// against it, so there is no such window here. It also makes the veto correct
// for a pipeline that is torn down and respawned mid-verdict, and it keeps one
// source of truth: pushDesired, pullDesired, the maintenance drain and the
// loops all read the same map.
func (h *SyncHandler) outboundVeto(notespaceID string) func() bool {
	return func() bool {
		_, contested := h.isContested(notespaceID)
		return contested
	}
}

// ClearContested records that a contested notespace has been adopted.
func (h *SyncHandler) ClearContested(notespaceID string) {
	h.parkMu.Lock()
	delete(h.contested, notespaceID)
	h.parkMu.Unlock()
}

// ContestedNotespaces returns id -> reason for every notespace currently
// refusing incoming applies.
func (h *SyncHandler) ContestedNotespaces() map[string]string {
	h.parkMu.Lock()
	defer h.parkMu.Unlock()
	out := make(map[string]string, len(h.contested))
	for id, entry := range h.contested {
		out[id] = entry.Reason
	}
	return out
}

// ContestedDetails returns the full verdicts, sorted by id — the structured
// view the adoption surfaces read.
func (h *SyncHandler) ContestedDetails() []ContestedNotespace {
	h.parkMu.Lock()
	entries := maps.Clone(h.contested)
	h.parkMu.Unlock()
	out := make([]ContestedNotespace, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b ContestedNotespace) int {
		return strings.Compare(a.NotespaceID, b.NotespaceID)
	})
	return out
}

// AdoptContested is the operator's adoption: it records the durable receipt
// (so a restart does not re-contest a decision already made) and clears the
// verdict, which lets the next reconcile pass restore the pull loop.
//
// The receipt is written FIRST. A cleared verdict with no receipt would let the
// pull loop resume and then re-contest itself on its next batch, which reads to
// an operator as an adoption that silently did nothing.
func (h *SyncHandler) AdoptContested(notespaceID string) (ContestedNotespace, string, error) {
	if notespaceID == "" {
		return ContestedNotespace{}, "", fmt.Errorf("adoption requires a notespace id")
	}
	h.parkMu.Lock()
	entry, ok := h.contested[notespaceID]
	h.parkMu.Unlock()
	if !ok {
		// Wrapped in the sentinel so the HTTP layer can keep its promise that a
		// script can tell the operator's mistake from a broken daemon.
		return ContestedNotespace{}, "", fmt.Errorf("notespace %s is not contested; nothing to adopt: %w", notespaceID, syncdb.ErrNotContested)
	}
	receipt, err := syncdb.RecordAdoption(notespaceID, entry.Root, entry.Detail)
	if err != nil {
		return entry, "", fmt.Errorf("record adoption receipt for %s: %w", notespaceID, err)
	}
	h.ClearContested(notespaceID)
	// The case has an explicit positive resolution, and the operator has just
	// been told incoming writes resume; leaving the artifact in place would
	// have `grove sync conflicts` still reporting it. Best effort: the receipt
	// is the decision, and a stale artifact must not fail an adoption that
	// already took effect.
	if err := syncdb.RetireNotespaceConflict(notespaceID, syncdb.ConflictKindAdoption); err != nil {
		h.ulog.Warn("adoption receipt written, but its conflict artifact could not be retired").
			Field("notespace_id", notespaceID).Err(err).Log(h.baseCtx)
	}
	h.ulog.Info("notespace adopted: incoming applies resume").
		Field("notespace_id", notespaceID).
		Field("root", entry.Root).
		Field("receipt", receipt).Log(h.baseCtx)
	// Reconcile now rather than on the next transport tick: adoption is an
	// interactive act and the operator is watching for the pull loop to come
	// back.
	go h.ensurePipelines()
	return entry, receipt, nil
}

// markContestedFromPull is the pull gate's callback: the gate has already
// withheld the batch, written the conflicts-feed artifact and broadcast the
// SSE update, so all that is left here is installing the verdict that keeps the
// pull pipeline down until the operator adopts.
func (h *SyncHandler) markContestedFromPull(root string, evidence syncdb.AdoptionEvidence) {
	h.markContested(evidence.Contest(root))
}

func (h *SyncHandler) isContested(id string) (string, bool) {
	h.parkMu.Lock()
	defer h.parkMu.Unlock()
	entry, ok := h.contested[id]
	return entry.Reason, ok
}

// pullDesired is the pull-side desired state for a resolved notespace: the
// recorded subscription's pull flag, minus the W3.5 contested veto.
//
// It exists so the code that SPAWNS a pull loop (startPipeline) and the code
// that decides a running pipeline is healthy (stopUndesired) read one
// definition. While only the spawn side had one, `contested` and `pull` were
// enforced exclusively at pipeline birth: consulted once, never compared again.
func (h *SyncHandler) pullDesired(r resolvedNotespace) bool {
	if r.sub == nil || !r.sub.Pull {
		return false
	}
	_, contested := h.isContested(r.id)
	return !contested
}

// pushDesired is the push side of the same question: a recorded subscription
// pushes — there is no `push` flag, push is what a subscription IS — unless the
// W3.5 gate has contested it.
//
// The contested veto belongs on BOTH sides, and the one-sided version was a
// hazard rather than a preference. The gate exists for a collision between two
// machines' copies of the same notespace, and it was protecting only the
// machine that raised it:
//
//  1. B holds pre-existing un-synced `plans/x.md`; A's copy in the shared
//     notebook differs.
//  2. B pulls. The pre-apply gate sees the divergence, withholds the batch,
//     writes the evidence, marks B contested. B's tree is protected.
//  3. B's push loop and anti-entropy pass — started unconditionally, in the
//     same reconcile, with no ordering between them and the pull that
//     contests — upload B's version. The server takes it: its only share-state
//     gate is ApplyPush, and B's notebook is shared. Anti-entropy is the wider
//     hole of the two, because walkLocalTree seeds every UNTRACKED local file
//     into the outbox, which is exactly what a machine with pre-existing notes
//     is full of.
//  4. A pulls, and A's `plans/x.md` is replaced by content no operator ever
//     adopted.
//
// The evidence-based decision W3.5 exists to offer was thereby pre-empted, in
// favour of whichever machine joined last, before anyone was asked. So a
// contested notespace now moves in neither direction: nothing is written into
// its tree, and nothing leaves it.
//
// The outbox is PARKED rather than drained by this — local edits keep queuing
// (the watch stays bound, since the pipeline is still installed at its root),
// and adoption releases them. Withholding push is not discarding local work; it
// is holding it until the operator says which copy wins.
//
// This function answers the question at pipeline BIRTH and at reconcile time,
// which is necessary and was not sufficient: the pipeline that raises a
// contest is normally one that is already pushing, and it went on pushing
// until the reconcile arrived — 9-20s in the field, enough to publish the
// disputed tree. The running loops therefore consult the same verdict
// themselves, per outbox batch and per anti-entropy pass; see outboundVeto.
func (h *SyncHandler) pushDesired(r resolvedNotespace) bool {
	if r.sub == nil {
		return false
	}
	_, contested := h.isContested(r.id)
	return !contested
}

// bindWatchIdentities stamps the immutable id onto every watch entry whose
// directory belongs to a running, registered pipeline. Until a watch carries an
// id, flush refuses to capture through it — which is what keeps a parked
// duplicate and an unregistered root out of the outbox, and what makes display
// names unusable as DB/wire keys.
func (h *SyncHandler) bindWatchIdentities() {
	h.pipelinesMu.Lock()
	roots := make(map[string]string, len(h.pipelines))
	for id, state := range h.pipelines {
		roots[state.root] = id
	}
	h.pipelinesMu.Unlock()

	h.pathsMutex.Lock()
	defer h.pathsMutex.Unlock()
	for _, watch := range h.watchedPaths {
		// A watch whose transport was torn down (subscription removed, root
		// re-pointed, duplicate parked) loses its binding here, which stops
		// capture through it on the next flush.
		watch.notespace = roots[watch.root]
	}
}

// stopUndesired cancels transports that recorded config no longer wants, and
// transports whose notespace has moved to a different root. Stopped pipelines
// move to `draining`; startPipeline will not replace one until it is gone.
func (h *SyncHandler) stopUndesired(resolved map[string]resolvedNotespace, desired map[string]string, generation uint64) {
	desiredRoots := make(map[string]bool, len(desired))
	for _, root := range desired {
		desiredRoots[root] = true
	}

	// Desired pull AND push state per id, computed before pipelinesMu is taken
	// so the health test below needs no lock of its own (parkMu under
	// pipelinesMu is the established order, but not needing it at all is
	// better).
	wantPull := make(map[string]bool, len(resolved))
	wantPush := make(map[string]bool, len(resolved))
	for id, r := range resolved {
		wantPull[id] = h.pullDesired(r)
		wantPush[id] = h.pushDesired(r)
	}

	type stopping struct {
		id     string
		state  *pipelineState
		reason string
		toRoot string
	}
	var stops []stopping

	h.pipelinesMu.Lock()
	for id, state := range h.pipelines {
		if want, ok := resolved[id]; ok {
			// Health is the root AND BOTH directions. Comparing only the root
			// made a running pipeline healthy forever regardless of what
			// config or the contested set now said, which falsified W3.5's
			// contract in both directions (MarkContested took effect only if
			// it beat the first reconcile) and let a subscription downgraded
			// to pull = false keep writing into the tree until a restart.
			//
			// Push is compared for the same reason, and it is the half that
			// matters most for a contest raised by the pull loop of a pipeline
			// already running: without it, a notespace marked contested at
			// 10:00:01 would keep uploading the disputed tree until something
			// unrelated re-rooted it.
			if samePhysicalPath(want.root, state.root) && state.pull == wantPull[id] && state.push == wantPush[id] {
				continue // healthy
			}
			if samePhysicalPath(want.root, state.root) {
				stops = append(stops, stopping{id: id, state: state, reason: "push/pull state changed"})
				continue
			}
			stops = append(stops, stopping{id: id, state: state, reason: "re-root", toRoot: want.root})
			continue
		}
		// Not resolved this pass. If the root is still desired, the stamp was
		// merely unreadable on this tick (or its id is parked behind another
		// root); keep the transport rather than flapping on a transient error.
		if desiredRoots[state.root] && !h.isParkedRoot(state.root) {
			continue
		}
		stops = append(stops, stopping{id: id, state: state, reason: "subscription removed"})
	}
	for _, stop := range stops {
		delete(h.pipelines, stop.id)
		delete(h.aePasses, stop.id)
		h.draining[stop.id] = stop.state
	}
	h.pipelinesMu.Unlock()

	slices.SortFunc(stops, func(a, b stopping) int { return strings.Compare(a.id, b.id) })
	for _, stop := range stops {
		stop.state.cancel()
		entry := h.ulog.Info("sync transport stopped").
			Field("notespace_id", stop.id).
			Field("root", stop.state.root).
			Field("reason", stop.reason).
			Field("generation", generation)
		if stop.toRoot != "" {
			entry = entry.Field("new_root", stop.toRoot)
		}
		entry.Log(h.baseCtx)
	}
}

// reclaimDrained drops pipelines whose goroutines have all returned from the
// draining map.
//
// startPipeline only ever reclaimed an entry when the SAME id became desired
// again, so a subscription removed for good leaked its pipelineState and its
// cancel closure permanently — and resetTransport moves every pipeline into
// draining at once, so an auth-reset cycle on a shrinking config accumulated
// them. Sweeping at the top of each pass also makes the map mean what its name
// says: what is left in it is still draining, which is the state worth
// escalating about.
func (h *SyncHandler) reclaimDrained() {
	h.pipelinesMu.Lock()
	defer h.pipelinesMu.Unlock()
	for id, state := range h.draining {
		if state.stopped() {
			delete(h.draining, id)
		}
	}
}

// isParkedRoot reports whether a root lost its identity to a duplicate this
// pass — a running pipeline on a parked root must stop.
func (h *SyncHandler) isParkedRoot(root string) bool {
	h.parkMu.Lock()
	defer h.parkMu.Unlock()
	for _, entry := range h.parked {
		if samePhysicalPath(entry.Root, root) {
			return true
		}
	}
	return false
}

// startPipeline registers a notespace and spawns its push / pull /
// anti-entropy loops, unless one is already running for it or a previous one
// is still draining.
func (h *SyncHandler) startPipeline(client *syncdb.Client, db *syncdb.DB, r resolvedNotespace, generation uint64) {
	h.pipelinesMu.Lock()
	if running := h.pipelines[r.id]; running != nil {
		h.pipelinesMu.Unlock()
		return
	}
	if old := h.draining[r.id]; old != nil {
		if !old.stopped() {
			old.drainWaits++
			waits := old.drainWaits
			h.pipelinesMu.Unlock()
			// A drain that never completes leaves the notespace with no
			// transport at all, and the wait is unbounded by design (a pull
			// sits in a 30s long-poll, so the first few passes waiting is
			// normal). What was missing is any way to tell "draining" from
			// "wedged": at Debug level the only evidence was invisible at
			// default verbosity. Escalate once the wait outlasts anything a
			// long-poll explains.
			entry := h.ulog.Debug("sync transport waiting for the previous pipeline to drain")
			if waits >= drainWaitWarnPasses {
				entry = h.ulog.Warn("sync transport blocked: the previous pipeline has not drained").
					Field("passes_waited", waits)
			}
			entry.Field("notespace_id", r.id).
				Field("old_root", old.root).
				Field("new_root", r.root).
				StructuredOnly().Log(h.baseCtx)
			return
		}
		delete(h.draining, r.id)
	}
	h.pipelinesMu.Unlock()

	if err := h.registerRoot(h.baseCtx, client, r.stamp, r.root); err != nil {
		h.ulog.Error("notespace registration failed; pipeline parked").Err(err).Field("root", r.root).Log(h.baseCtx)
		return
	}

	// registerRoot is a network round trip, and the caller's generation check
	// is per-iteration — taken BEFORE it. A reload landing inside this window
	// would otherwise install a pipeline stamped with a generation that is
	// already stale, routed from superseded config. The queued reconcile the
	// reload triggers heals it, but not before the wrong root has been pushed
	// from; re-checking here costs one atomic load.
	if h.configGeneration.Load() != generation {
		h.ulog.Debug("sync transport not installed: config generation advanced during registration").
			Field("notespace_id", r.id).
			Field("root", r.root).
			Field("generation", generation).
			StructuredOnly().Log(h.baseCtx)
		return
	}

	pctx, cancel := context.WithCancel(h.baseCtx)
	var wg sync.WaitGroup
	done := make(chan struct{})
	run := func(kind string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.runWithRecovery(pctx, r.id, kind, fn)
		}()
	}

	name, root, stamp := r.id, r.root, r.stamp
	sub := r.sub

	// The W3.5 verdict, read once for this pipeline. It vetoes BOTH directions:
	// see pushDesired for why a one-sided gate protected the machine that
	// raised the collision and decided the other side of it automatically.
	contestedReason, contested := h.isContested(name)
	push := h.pushDesired(r)
	pull := h.pullDesired(r)
	if contested {
		h.ulog.Warn("sync withheld in both directions: notespace is contested and not adopted yet").
			Field("notespace_id", name).
			Field("root", root).
			Field("reason", contestedReason).Log(h.baseCtx)
	}

	if push {
		h.startOutbound(pctx, run, db, client, name, root, stamp, sub)
	}

	if pull {
		idSub := *sub
		idSub.Name = name
		pullPipeline := syncdb.NewPullPipeline(&idSub, client, db, h.ulog)
		// Own-note guard (registry-role subscriptions only): an inbound
		// event for machines/<our id>.md cannot be a legitimate
		// replication of our own write, because the registry is
		// single-writer. Dropped and surfaced, never applied — see
		// PullPipeline.guardOwnRegistryNote for why this is detection
		// rather than prevention under the interim trust model.
		pullPipeline.OwnMachineID = machine.ID()
		pullPipeline.OnRegistryForeignWrite = func(ws, path, detail string) {
			h.broadcastConflict(&store.SyncConflictPayload{
				Kind:        syncdb.ConflictKindRegistryForeignWrite,
				NotespaceID: ws,
				Path:        path,
				Detail:      detail,
			})
		}
		pullPipeline.OnConflict = func(kind, ws, path, documentID, detail string) {
			h.broadcastConflict(&store.SyncConflictPayload{Kind: kind, NotespaceID: ws, NotespaceName: stamp.Name, Path: path, DocumentID: documentID, Detail: detail})
		}
		// W3.5: the gate's verdict becomes this daemon's, and it takes effect
		// before this callback returns. Marking installs the verdict the
		// outbound loops of THIS pipeline consult per batch (outboundVeto), so
		// the batch the gate withheld is the last one this pipeline ever
		// considers in either direction — the next reconcile tears the
		// transport down, but nothing waits for it. The one thing that could
		// still be in flight is a push whose HTTP request was already on the
		// wire when the gate closed; everything the loops have not yet sent
		// stays parked in the outbox.
		pullPipeline.OnContested = func(_ string, evidence syncdb.AdoptionEvidence) {
			h.markContestedFromPull(root, evidence)
		}
		run("pull", func() error { return pullPipeline.RunPullLoop(pctx, root) })
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	h.pipelinesMu.Lock()
	h.pipelines[name] = &pipelineState{
		cancel: cancel, done: done, root: root, pull: pull, push: push, generation: generation,
	}
	h.pipelinesMu.Unlock()

	// Materialize the sync_state row immediately so /api/sync/status
	// reflects the subscription as soon as transport starts (readiness
	// probes key on this; rows otherwise appear only on first activity).
	if cur, err := db.GetNotespaceCursor(name); err == nil {
		_ = db.UpdateNotespaceCursor(name, cur)
	}

	// Hydration and transport log lines name the RESOLVED ROOT (W3.3): a
	// re-root is otherwise indistinguishable from a restart in the log.
	h.ulog.Info("sync transport started").
		Field("notespace_id", name).
		Field("notespace_name", stamp.Name).
		Field("root", root).
		Field("push", push).
		Field("pull", pull).
		Field("generation", generation).
		StructuredOnly().Log(pctx)
}

// startOutbound spawns everything that sends local content to the server: the
// push loop, and the anti-entropy pass that feeds it.
//
// The two are started together, and withheld together, because they are one
// direction. Anti-entropy is not a "reconcile" that happens to push — it is the
// push side's own sweep (it never writes the local tree; see the cursor note in
// AntiEntropyPass.Run), and walkLocalTree seeds untracked local files straight
// into the outbox. Leaving it running for a contested notespace while holding
// back the push loop would queue exactly the content under dispute, to be
// uploaded the moment anything drained the outbox.
func (h *SyncHandler) startOutbound(
	pctx context.Context,
	run func(kind string, fn func() error),
	db *syncdb.DB,
	client *syncdb.Client,
	name, root string,
	stamp *notespacepkg.NotespaceStamp,
	sub *config.SyncWorkspace,
) {
	push := syncdb.NewPushPipeline(db, client, name, h.ulog, syncdb.PushConfig{})
	// The W3.5 veto, live rather than decided at birth. `push` here is already
	// the answer to "should this loop exist at all", taken one pass ago; this
	// is the answer to "may this batch leave", taken now. The pull loop spawned
	// below is what usually raises the verdict, and it raises it against the
	// pipeline these two loops belong to.
	push.Withheld = h.outboundVeto(name)
	// Surface server-ceiling oversize skips as a sync_conflict SSE update
	// (convertToAPIUpdate already forwards UpdateSyncConflict). The quiet
	// per-notespace MaxFileSize skip stays in flush; this is the loud one.
	push.OnOversizeSkipped = func(ws, path string, size, limit int64) {
		h.broadcastConflict(&store.SyncConflictPayload{
			Kind:        "oversize_skipped",
			NotespaceID: ws,
			Path:        path,
			Detail:      fmt.Sprintf("%d bytes exceeds server blob ceiling %d", size, limit),
		})
	}
	// Surface a push-side divergence (S5): the merged server head was pushed
	// but the local file was left untouched, so it lags until the user runs
	// `nb sync adopt`. Same SSE surfacing as an oversize skip.
	push.OnDiverged = func(ws, path string) {
		h.broadcastConflict(&store.SyncConflictPayload{
			Kind:        "diverged",
			NotespaceID: ws,
			Path:        path,
			Detail:      "local file lags the merged server head; run `nb sync adopt` to take it",
		})
	}
	push.OnConflict = func(kind, ws, path, documentID, detail string) {
		h.broadcastConflict(&store.SyncConflictPayload{Kind: kind, NotespaceID: ws, NotespaceName: stamp.Name, Path: path, DocumentID: documentID, Detail: detail})
	}
	run("push", func() error { return push.RunPushLoop(pctx, root) })

	// Build the reconcile with the same per-notespace DocSpace the watcher
	// uses, so walk coverage and reconcile coverage judge the doc space
	// identically.
	ae := syncdb.NewAntiEntropyPass(db, client, name, root, syncdb.NewDocSpace(sub), h.ulog, syncdb.AntiEntropyConfig{})
	// Same veto as the push loop: the sweep is the other half of this
	// direction, and walkLocalTree is the leg that seeds the disputed tree.
	ae.Withheld = h.outboundVeto(name)
	// A recreated server is detected by whichever pass handshakes first,
	// but CheckServerEpoch voids EVERY notespace's synced state and
	// clears their outboxes. Fan the sweep out so the others re-push in
	// this cycle rather than sitting empty until their own hourly tick.
	ae.OnEpochReset = h.kickAntiEntropyExcept
	run("anti-entropy", func() error {
		// One immediate pass (initial reconciliation), then the loop. A failed
		// initial pass is logged and fallen THROUGH, matching the policy
		// RunAntiEntropyLoop already applies to every later pass ("continue
		// polling on error"). Returning here instead exited the goroutine
		// before the loop was ever entered, so a single transient refusal —
		// and W3.2 gave Run a brand-new one in RequireNotespaceRoot, for a
		// condition that is transient by construction (an unmounted volume, a
		// replica not yet materialized, a notebook about to be pulled) — left
		// the notespace running push and pull with NO reconciliation for the
		// life of the process. Nothing would have noticed: the reconcile pass
		// sees a live pipeline at the right root, and `done` never closes
		// because push and pull are still running, so the drain gate cannot
		// recycle it either.
		if err := ae.Run(pctx); err != nil {
			if pctx.Err() != nil {
				return err // cancelled, not failed: do not enter the loop
			}
			h.ulog.Warn("initial anti-entropy pass failed; continuing into the periodic loop").
				Field("notespace_id", name).
				Field("root", root).
				Err(err).Log(pctx)
		}
		return ae.RunAntiEntropyLoop(pctx)
	})

	// Registered only when the pass is actually running. KickAntiEntropy and
	// the maintenance drain both fan out over this map, so an entry for a
	// notespace whose outbound side is withheld would be a second way to sweep
	// a contested tree into the outbox.
	h.pipelinesMu.Lock()
	h.aePasses[name] = ae
	h.pipelinesMu.Unlock()
}
