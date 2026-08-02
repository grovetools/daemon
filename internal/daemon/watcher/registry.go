package watcher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/machine"
	"github.com/grovetools/core/pkg/registry"
	"github.com/grovetools/core/version"
)

// registryScanInterval is how often the writer RE-EVALUATES this machine's
// presence note. It is not how often it writes one.
//
// The contract names three triggers: daemon start, structural change, and a
// daily tick. Start and structural change are explicit (OnStart and
// kickRegistry). This ticker covers the third and, in practice, most of the
// second: a structural change the daemon never gets an event for — an
// ecosystem materialized by hand, a repo switched to another branch, a
// submodule added — is noticed at the next sweep instead of never.
//
// Re-evaluating hourly costs a config load and a stat-and-two-reads per repo,
// and writes NOTHING unless the rendered bytes actually differ, so the daily
// write cadence the contract specifies is preserved by the byte-compare rather
// than by the tick interval. Steady state stays ≤1 event row per machine per
// day.
const registryScanInterval = time.Hour

// startRegistryWriter launches the presence-note goroutine. Owned by the
// SyncHandler because the note's location is derived from a sync subscription
// (syntheticNodeFor/nodeWorkspaceRoot) and its origin_id comes from sync.db —
// both of which the handler already owns and neither of which anything else
// in the daemon can resolve.
func (h *SyncHandler) startRegistryWriter(ctx context.Context) {
	go h.registryLoop(ctx)
}

// kickRegistry asks for an out-of-band re-evaluation after a structural
// change. Non-blocking and coalescing: a burst of config reloads produces one
// extra pass, not one per reload.
func (h *SyncHandler) kickRegistry() {
	if h.registryKick == nil {
		return
	}
	select {
	case h.registryKick <- struct{}{}:
	default: // a pass is already pending; it will see the new state
	}
}

func (h *SyncHandler) registryLoop(ctx context.Context) {
	interval := h.registryInterval
	if interval <= 0 {
		interval = registryScanInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// Trigger 1 (daemon start) is this first iteration; every later one is
		// the tick or a kick.
		h.writeRegistryNote(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-h.registryKick:
		}
	}
}

// registryClock is the writer's time source, overridable in tests. Only the
// DAY matters (last_seen is day-resolution), so nothing here is sensitive to
// sub-second timing.
func (h *SyncHandler) registryClock() time.Time {
	if h.registryNow != nil {
		return h.registryNow()
	}
	return time.Now()
}

// writeRegistryNote renders this machine's presence note and writes it only if
// the bytes changed — the whole point of the exercise.
//
// # Source-side suppression
//
// The skip happens HERE, at the source, not downstream. It has to: the
// watcher's flush enqueues an outbox row on every fsnotify flush, and the only
// demonstrable hash-gate downstream is the anti-entropy sweep's
// diskHash == doc.LastSyncedHash comparison — which never runs on a file that
// was rewritten with identical content between passes. A writer that rewrote
// the note every hour would therefore push 24 events a day per machine, every
// one of them a no-op. Comparing rendered bytes to disk before writing is what
// makes the steady-state cost zero.
//
// # rev and last_seen
//
// Both change only when something ELSE changed, or when the day rolled over.
// That is why the candidate is rendered with the PREVIOUS rev and last_seen:
// if the note is otherwise identical, the candidate is byte-identical to disk
// and the write is skipped. Bumping rev first would make every note differ
// from itself and defeat the comparison — the counter would become the only
// reason to write, forever.
//
// # Notebook writes
//
// This is the one place the sync handler writes into the notebook tree. The
// notebook-read-only rule protects the user's notes from the sync machinery;
// the presence note is not the user's note, it is this machine's own document
// in a workspace reserved for exactly that, and it is single-writer by
// construction.
func (h *SyncHandler) writeRegistryNote(ctx context.Context) {
	sub := registry.Subscription(h.syncConfigSnapshot())
	if sub == nil {
		return // no registry subscription: the whole feature stays dark
	}
	id := machine.ID()
	if id == "" {
		h.warnRegistryOnce(ctx, "registry note skipped: this machine has no identity", nil)
		return
	}
	root := h.nodeWorkspaceRoot(h.syntheticNodeFor(sub.Name))
	if root == "" {
		h.warnRegistryOnce(ctx,
			fmt.Sprintf("registry note skipped: cannot resolve a local root for workspace %q", sub.Name), nil)
		return
	}

	originID := ""
	if db := h.database(); db != nil {
		originID = db.OriginID()
	}
	// A malformed machine.toml degrades to "nothing declared" rather than
	// taking presence dark: the operator sees the real error from
	// `grove machine status`, and a machine with an unreadable intent file is
	// still a machine worth seeing in the registry.
	machineCfg, err := config.LoadMachineConfig()
	if err != nil {
		h.warnRegistryOnce(ctx, "registry note: machine config unreadable, recording identity only", err)
		machineCfg = nil
	}

	note := registry.Build(registry.BuildInput{
		MachineID:     id,
		Name:          config.ResolveMachineName(),
		OriginID:      originID,
		GrovedVersion: version.Version,
		Machine:       machineCfg,
		Subscriptions: h.subscriptionsSnapshot(),
	})

	notePath := filepath.Join(root, filepath.FromSlash(registry.NotePath(id)))
	previous, readErr := os.ReadFile(notePath) //nolint:gosec // path derived from configured notebook root
	var prevNote *registry.Note
	if readErr == nil {
		prevNote, _ = registry.ParseNote(previous) // an unreadable own note is repaired below
	}

	today := registry.Today(h.registryClock())
	if prevNote != nil {
		// Render at the previous rev/last_seen so an unchanged machine
		// produces byte-identical output.
		note.Rev, note.LastSeen = prevNote.Rev, prevNote.LastSeen
		if bytes.Equal(note.Render(), previous) && prevNote.LastSeen == today {
			return // nothing changed and the day has not rolled over
		}
		note.Rev = prevNote.Rev + 1
	} else {
		note.Rev = 1
	}
	note.LastSeen = today

	if err := writeNoteAtomically(notePath, note.Render()); err != nil {
		h.ulog.Warn("failed to write registry presence note").
			Field("path", notePath).Err(err).Log(ctx)
		return
	}
	h.registryWarned = false
	h.ulog.Info("registry presence note written").
		Field("workspace", sub.Name).
		Field("path", registry.NotePath(id)).
		Field("rev", note.Rev).
		StructuredOnly().Log(ctx)
}

// writeNoteAtomically replaces a note through a temp file in the same
// directory, so a crash mid-write cannot leave a truncated document for the
// push pipeline to replicate.
//
// The temp file is DOT-PREFIXED on purpose: SyncHandler.MatchesEvent drops any
// path whose basename starts with a dot, so the intermediate file never
// becomes an outbox row. Only the final rename produces a syncable event.
func writeNoteAtomically(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".machine-note-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// warnRegistryOnce logs a registry-writer problem at most once per run of
// problems. The loop re-evaluates hourly and these conditions (no identity, an
// unresolvable root) persist, so an unconditional warn would be an hourly log
// line about a state the operator already knows.
func (h *SyncHandler) warnRegistryOnce(ctx context.Context, msg string, err error) {
	if h.registryWarned {
		return
	}
	h.registryWarned = true
	entry := h.ulog.Warn(msg)
	if err != nil {
		entry = entry.Err(err)
	}
	entry.Log(ctx)
}

// syncConfigSnapshot returns the live sync config under the same lock every
// other reader uses.
func (h *SyncHandler) syncConfigSnapshot() *config.SyncConfig {
	h.syncCfgMu.RLock()
	defer h.syncCfgMu.RUnlock()
	return h.syncCfg
}
