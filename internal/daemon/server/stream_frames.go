package server

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grovetools/daemon/internal/daemon/store"
	"github.com/grovetools/daemon/internal/daemon/telemetry"
)

// The marshal-once frame cache for /api/stream.
//
// One store publish reaches every subscriber's own request goroutine, and each
// of those goroutines used to run the whole serialize pipeline independently:
// convertToAPIUpdate, then json.Marshal. The output is byte-identical across
// subscribers — the fan-out is the only reason it was produced more than once —
// so a host with 40 streams paid 40 conversions and 40 marshals per event, and
// on the subscribe-time snapshot (the largest frame this endpoint ever writes)
// 40 simultaneous copies of the whole enriched-workspace map.
//
// Every published update carries a unique store sequence (recordLocked stamps
// it), which makes seq the natural cache key: two subscribers holding the same
// Seq are holding the same bytes, by construction. The first goroutine to reach
// a sequence does the work; the rest read the shared, immutable []byte and
// write it to their own connection.
//
// Three properties this deliberately keeps:
//
//   - Filtering still happens BEFORE marshalling. Conversion is cached
//     unconditionally (every subscriber already ran it before its type filter
//     could be evaluated — the type is a field of the converted value), but the
//     marshal is only ever requested by a subscriber that already passed
//     ?types= and ?paths=. A lone narrow subscriber does not resurrect the cost
//     job 85 removed.
//   - Path-filtered subscribers whose allow-list actually PRUNES rows marshal
//     their own pruned copy, as before. applyStreamFilter returns the input
//     pointer unchanged when nothing was pruned, so pointer identity is the
//     exact test for "this subscriber's frame is the shared frame".
//   - Per-subscriber back-pressure, watermark and disconnect semantics are
//     untouched: this caches bytes, not delivery.
//
// A correctness note in passing: cached bytes freeze the payload as of the
// first marshal. Today's subscribers race store-side in-place payload mutation
// and can serialize divergent frames from the same Seq; sharing one marshal
// removes that skew.

const (
	// frameCacheSlots is the live-frame ring depth. Sequences are monotonic, so
	// seq&mask gives natural oldest-first eviction with no LRU bookkeeping, and
	// a subscriber lagging further behind than the ring simply misses and
	// marshals for itself.
	//
	// The depth that matters is not one subscriber's convert→marshal gap (that
	// is microseconds, back-to-back in the same loop iteration) but the SPREAD
	// between subscribers: while the fastest is at sequence S the slowest may
	// still be at S-k, and every slot the fast one recycles under the slow one
	// costs a re-marshal. Measured on a 700-workspace scratch daemon with 40
	// subscribers, a 32-slot ring re-marshalled 1.9× per publish (95.2% hit
	// rate) for exactly that reason. The ring is cheap — a slot is a mutex and
	// four words, and what it RETAINS is capped separately by the byte budget
	// below — so it is sized for the spread rather than for the gap.
	frameCacheSlots = 256
	frameCacheMask  = frameCacheSlots - 1

	// frameCacheDefaultBudget bounds what the ring RETAINS, because slot count
	// alone does not: a ring of multi-megabyte workspaces frames is real heap.
	// Over budget, old slots are released; the cache degrades to marshalling
	// more often, never to wrong bytes. Ordinary delta frames are kilobytes, so
	// this binds only when frames are unusually large — which is when it should.
	frameCacheDefaultBudget = 16 << 20

	// initialFrameCacheEntries / initialFrameCacheTTL bound the snapshot cache.
	// It exists for reconnect storms — many clients subscribing within a few
	// hundred milliseconds of a daemon becoming reachable — so a short TTL
	// captures the storm while keeping the largest frame in the daemon from
	// being retained for any meaningful time. The key includes the path filter
	// because a filtered snapshot is a different frame.
	initialFrameCacheEntries = 2
	initialFrameCacheTTL     = time.Second
)

// frameCache is the per-Server cache. The zero value is unusable; use
// newFrameCache.
type frameCache struct {
	// enabled is the escape hatch. Disabled, every method falls through to
	// exactly the pre-cache behavior (convert and marshal per caller), which is
	// what makes an A/B on one binary possible.
	enabled bool
	budget  int64

	slots [frameCacheSlots]frameSlot
	// retained is the summed length of the byte slices the ring is holding.
	retained atomic.Int64

	initialMu      sync.Mutex
	initialEntries []*initialFrame
}

// frameSlot holds one sequence's shared work. Its mutex is held across the
// conversion and across the marshal, which is what makes the cache
// single-flight: concurrent subscribers of the same sequence block on the first
// one rather than racing it and each producing a copy.
type frameSlot struct {
	mu   sync.Mutex
	seq  uint64
	api  *apiStateUpdate
	ok   bool
	data []byte
	err  error
	// marshaled distinguishes "not marshalled yet" from "marshalled to nothing":
	// data is nil in both cases when err is set.
	marshaled bool
}

// initialFrame is one cached subscribe-time snapshot.
type initialFrame struct {
	key  string
	data []byte
	sent bool
	err  error
	at   time.Time
}

// newFrameCache reads the escape hatch and the budget from the environment.
//
// The knobs live in the environment rather than in core's DaemonConfig for the
// same reason as GROVE_SWEEP_TIERED: a field there invalidates grove's
// generated config schema in a third repo, and these are dials you turn while
// measuring, not settings you keep.
//
//	GROVE_SSE_FRAME_CACHE=0            restore per-subscriber marshalling
//	GROVE_SSE_FRAME_CACHE_BUDGET=<n>   retained-bytes budget for the live ring
func newFrameCache() *frameCache {
	c := &frameCache{enabled: true, budget: frameCacheDefaultBudget}
	if v := os.Getenv("GROVE_SSE_FRAME_CACHE"); v == "0" || strings.EqualFold(v, "false") {
		c.enabled = false
	}
	if n, err := strconv.ParseInt(os.Getenv("GROVE_SSE_FRAME_CACHE_BUDGET"), 10, 64); err == nil && n > 0 {
		c.budget = n
	}
	return c
}

// convert returns the wire shape of one published update, shared across every
// subscriber that reaches the same sequence. ok is false when the update has no
// wire mapping (noteUnconvertedUpdate has already said so, once per type).
//
// The returned value is immutable and shared: callers may read it and may hand
// it to applyStreamFilter (which shallow-copies before pruning), but must never
// write through it.
func (c *frameCache) convert(u store.Update) (*apiStateUpdate, bool) {
	if c == nil || !c.enabled || u.Seq == 0 {
		api := convertToAPIUpdate(u)
		return api, api != nil
	}
	slot := &c.slots[u.Seq&frameCacheMask]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.seq == u.Seq {
		return slot.api, slot.ok
	}
	c.resetLocked(slot)
	api := convertToAPIUpdate(u)
	slot.seq, slot.api, slot.ok = u.Seq, api, api != nil
	return api, slot.ok
}

// marshal returns the JSON for a frame convert produced, marshalling it at most
// once per sequence. api must be the exact pointer convert returned for seq;
// anything else (a pruned per-subscriber copy) has to be marshalled by its
// owner and must not be attributed to the shared sequence.
func (c *frameCache) marshal(seq uint64, api *apiStateUpdate) ([]byte, error) {
	if c == nil || !c.enabled || seq == 0 {
		// Counted as a miss so sse.marshal.cache_misses reads "marshals
		// performed" in both arms of an A/B — with the cache off it is the
		// per-subscriber count, and the two numbers are directly comparable.
		telemetry.SSEMarshalCacheMisses.Inc()
		return json.Marshal(api)
	}
	slot := &c.slots[seq&frameCacheMask]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	// Recycled or never populated: the caller's frame is no longer the one this
	// slot describes, so marshal it privately rather than poisoning the slot.
	if slot.seq != seq || slot.api != api {
		telemetry.SSEMarshalCacheMisses.Inc()
		return json.Marshal(api)
	}
	if slot.marshaled {
		telemetry.SSEMarshalCacheHits.Inc()
		telemetry.SSEMarshalSharedBytes.Add(int64(len(slot.data)))
		return slot.data, slot.err
	}
	data, err := json.Marshal(api)
	slot.data, slot.err, slot.marshaled = data, err, true
	c.retained.Add(int64(len(data)))
	telemetry.SSEMarshalCacheMisses.Inc()
	c.trim(seq)
	return data, err
}

// resetLocked drops whatever a slot was holding before it is reused. The
// caller holds slot.mu.
func (c *frameCache) resetLocked(slot *frameSlot) {
	if slot.marshaled {
		c.retained.Add(-int64(len(slot.data)))
	}
	slot.api, slot.ok, slot.data, slot.err, slot.marshaled = nil, false, nil, nil, false
}

// trim releases old slots until the ring is back inside its byte budget.
//
// It only ever touches slots it can lock without waiting and only ones strictly
// older than the sequence being served, so it can neither deadlock against a
// concurrent convert/marshal nor free a frame someone is still assembling. An
// over-budget cache that cannot free anything right now stays over budget until
// the next marshal, which is the correct trade: this is a memory bound, not an
// invariant.
func (c *frameCache) trim(current uint64) {
	if c.retained.Load() <= c.budget {
		return
	}
	for i := range c.slots {
		if c.retained.Load() <= c.budget {
			return
		}
		slot := &c.slots[i]
		if slot == &c.slots[current&frameCacheMask] {
			continue
		}
		if !slot.mu.TryLock() {
			continue
		}
		if slot.marshaled && slot.seq < current {
			c.resetLocked(slot)
			slot.seq = 0
			telemetry.SSEFrameCacheEvicted.Inc()
		}
		slot.mu.Unlock()
	}
}

// initialFrameKey identifies a subscribe-time snapshot: the store sequence it
// was built at, plus the path filter that shaped it. Two subscribers sharing
// both share the frame; a change to either produces different bytes.
func initialFrameKey(seq uint64, paths []string) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(seq, 10))
	for _, p := range paths {
		b.WriteByte('|')
		b.WriteString(p)
	}
	return b.String()
}

// initial returns the marshalled subscribe-time snapshot for one key, building
// it through build at most once per TTL window. build reports whether there is
// a frame to send at all (the path filter can drop the whole snapshot); that
// verdict is cached alongside the bytes, since re-deciding it means rebuilding
// the same frame.
//
// The mutex is held across build on purpose. Serializing snapshot construction
// IS the win: a reconnect storm is precisely many clients arriving at once, and
// letting them queue behind one marshal is the difference between one copy of
// the largest frame in the daemon and one per client.
func (c *frameCache) initial(key string, build func() ([]byte, bool, error)) ([]byte, bool, error) {
	if c == nil || !c.enabled {
		telemetry.SSEInitialCacheMisses.Inc()
		return build()
	}
	c.initialMu.Lock()
	defer c.initialMu.Unlock()

	now := time.Now()
	live := c.initialEntries[:0]
	var hit *initialFrame
	for _, e := range c.initialEntries {
		if now.Sub(e.at) >= initialFrameCacheTTL {
			continue
		}
		live = append(live, e)
		if e.key == key {
			hit = e
		}
	}
	c.initialEntries = live
	if hit != nil {
		telemetry.SSEInitialCacheHits.Inc()
		return hit.data, hit.sent, hit.err
	}

	data, sent, err := build()
	telemetry.SSEInitialCacheMisses.Inc()
	c.initialEntries = append(c.initialEntries, &initialFrame{
		key: key, data: data, sent: sent, err: err, at: now,
	})
	if len(c.initialEntries) > initialFrameCacheEntries {
		c.initialEntries = c.initialEntries[len(c.initialEntries)-initialFrameCacheEntries:]
	}
	return data, sent, err
}
