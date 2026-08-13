package store

import (
	"encoding/binary"
	"hash/fnv"

	"github.com/grovetools/core/pkg/models"
)

// The note-index publish fence.
//
// Both note producers — the periodic NoteCollector and the note watcher's
// debounced rebuild — walk every workspace's notebook and hand the store a
// freshly allocated index of every note in the ecosystem (tens of thousands of
// entries, a double-digit number of megabytes at scale). They then published it
// unconditionally, on every scan, whether or not a single note had changed.
//
// That violated the producer rule the rest of the state gatherers already
// follow (see store/compare.go: no producer publishes a delta whose fields
// equal current state). It is expensive in a specific way: the 1024-slot replay
// ring retains published updates BY REFERENCE, so every republish handed the
// ring another whole map to keep alive, and the old one stayed pinned until the
// ring rotated past it.
//
// The fix is the statsInputDigest/statsSeq fence from the flow watcher, applied
// to the same shape of problem: fingerprint what was built, publish only when
// the fingerprint moved.

// noteIndexDigestOf fingerprints a note index.
//
// Order-independent by construction — the input is a map, and sorting tens of
// thousands of keys to get a stable walk order would cost more than the hash.
// Each entry is hashed with its key folded in, so no two entries can produce
// the same value and sum them to zero, and the entry count is mixed in so that
// a strict subset cannot collide with the whole.
//
// Every field that reaches a consumer participates: the index is a read model,
// and a field that changed without moving the digest would be a note edit that
// never reaches a client.
func noteIndexDigestOf(index map[string]*models.NoteIndexEntry) uint64 {
	var sum uint64
	for key, entry := range index {
		h := fnv.New64a()
		writeDigestString(h, key)
		if entry != nil {
			writeDigestString(h, entry.Path)
			writeDigestString(h, entry.Name)
			writeDigestString(h, entry.Title)
			for _, tag := range entry.Tags {
				writeDigestString(h, tag)
			}
			writeDigestString(h, entry.ID)
			writeDigestString(h, entry.PlanRef)
			writeDigestString(h, entry.PlanJob)
			writeDigestString(h, entry.Priority)
			writeDigestString(h, entry.Type)
			writeDigestString(h, entry.Group)
			writeDigestString(h, entry.Workspace)
			writeDigestString(h, entry.ContentDir)
			writeDigestUint(h, uint64(entry.Created.UnixNano()))
			writeDigestUint(h, uint64(entry.ModTime.UnixNano()))
		}
		sum += h.Sum64()
	}
	// Mix the count in last so an empty index and a whole-index deletion are
	// distinguishable from each other and from a subset.
	h := fnv.New64a()
	writeDigestUint(h, sum)
	writeDigestUint(h, uint64(len(index)))
	return h.Sum64()
}

// writeDigestString feeds a field into a digest with a separator, so that
// moving a character across a field boundary changes the result.
func writeDigestString(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(s))
	_, _ = h.Write([]byte{0})
}

// writeDigestUint feeds a fixed-width numeric field into a digest.
func writeDigestUint(h interface{ Write([]byte) (int, error) }, v uint64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	_, _ = h.Write(buf[:])
}
