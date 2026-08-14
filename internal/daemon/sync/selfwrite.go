package sync

import (
	"time"
)

// Self-write suppression: the pull apply's echo firewall.
//
// Every file the pull pipeline writes into the notespace tree is observed
// back through fsnotify by the watcher, whose debounced flush then reads the
// file AND the sync_documents row and decides whether to capture an outbox
// event. The flush's hash-gate (doc.ContentHash == disk hash) suppresses the
// echo — but only when the apply's own row bookkeeping has already committed.
// When the flush wins that race, or the apply's DB write failed after the
// file write landed (a half-applied event), the flush sees a dirty-looking
// file, enqueues the daemon's OWN apply-write for push-back (base_version 0 →
// parked manufactured conflict, forever), and stamps v-era bookkeeping over
// the row. Observed live: test drive S2.2, canary round-trip A→B wedged.
//
// This registry closes the race without ordering constraints: the apply
// registers (notespace, path, hash-of-bytes-written) BEFORE the write, and
// InsertAndEnqueue — the single seeding chokepoint shared by the watcher
// flush and the anti-entropy tree walk — drops any capture whose content is
// byte-identical to the registered write. The hash match is the safety
// argument: only PURE SERVER CONTENT is ever registered (fast-forward and
// create applies; never merge results, which carry an unpushed local half),
// so suppressed bytes are always bytes the server already holds — there is
// nothing to push, no matter how stale the registration is.
//
// A registration is superseded by observing a DIFFERENT hash for the path (a
// real local edit landed after the apply; from then on every capture must
// flow, including a later revert to the applied bytes) and expires after
// selfWriteTTL as backstop hygiene.
const selfWriteTTL = 5 * time.Minute

// selfWrite is one registered apply-write expectation.
type selfWrite struct {
	hash string
	at   time.Time
}

func selfWriteKey(notespace, path string) string {
	return notespace + "\x00" + path
}

// NoteSelfWrite registers content the pull apply is about to write at a
// notespace path, so the watcher's flush recognizes the resulting fsnotify
// event as the daemon's own write. Call it immediately BEFORE the file write
// — registering after would re-open the race this exists to close. Only
// register pure server content (see the package comment above).
func (d *DB) NoteSelfWrite(notespace, path, hash string) {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	if d.selfWrites == nil {
		d.selfWrites = make(map[string]selfWrite)
	}
	d.selfWrites[selfWriteKey(notespace, path)] = selfWrite{hash: hash, at: time.Now()}
}

// MatchesSelfWrite reports whether captured content at a notespace path is
// byte-identical to a live registered apply-write (the echo case — the caller
// must not enqueue it and must not touch the doc row, the apply owns that
// bookkeeping). A non-matching hash supersedes the registration: a real local
// edit has landed, and every capture after it — including a revert back to
// the applied bytes — is user intent that must flow.
func (d *DB) MatchesSelfWrite(notespace, path, hash string) bool {
	d.selfWritesMu.Lock()
	defer d.selfWritesMu.Unlock()
	key := selfWriteKey(notespace, path)
	entry, ok := d.selfWrites[key]
	if !ok {
		return false
	}
	if time.Since(entry.at) > selfWriteTTL || entry.hash != hash {
		delete(d.selfWrites, key)
		return false
	}
	return true
}
