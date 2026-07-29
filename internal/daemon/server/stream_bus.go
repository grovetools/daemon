package server

import (
	"context"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// streamLog is the package logger for the event-bus plumbing. The conversion
// path is a package-level function reached from tests without a *Server, so it
// cannot use s.ulog.
var streamLog = sync.OnceValue(func() *logging.UnifiedLogger {
	return logging.NewUnifiedLogger("groved.stream")
})

// The /api/stream contract, version 2.
//
// Historically the endpoint was an unfiltered firehose with no cursor: every
// subscriber decoded every frame, and a subscriber whose 100-deep channel
// filled (or that simply reconnected) had no way to learn what it missed.
// Three query parameters and one response header change that, all of them
// additive — an old client that passes nothing sees exactly the old behavior,
// and an old DAEMON that receives the new parameters ignores them, which is
// why the client must look for StreamFeaturesHeader rather than assume.
//
//	GET /api/stream                          unfiltered, from now (unchanged)
//	GET /api/stream?since=<seq>              resume after sequence <seq>
//	GET /api/stream?types=job_*,note_event   server-side glob filter
//
// Every frame carries "seq". Two control frames exist and are NEVER filtered
// by ?types= (they describe the stream, not the state): the "initial"
// snapshot — which IS filtered, being ordinary state — and "stream_gap".
const (
	// StreamFeaturesHeader advertises what this daemon's /api/stream supports,
	// as a comma-separated token list. Absent means a pre-hardening daemon:
	// frames carry no sequence, and since/types were ignored.
	StreamFeaturesHeader = "X-Grove-Stream-Features"
	// StreamRingHeader advertises the replay ring's bound, in updates.
	StreamRingHeader = "X-Grove-Stream-Ring"
	// StreamFeatures is this daemon's feature token list.
	StreamFeatures = "seq,since,types"
	// StreamGapUpdateType is the control frame emitted when a ?since= cursor
	// could not be honored exactly. The client must snapshot-reconcile.
	StreamGapUpdateType = "stream_gap"
)

// apiStreamGap is the payload of a stream_gap control frame. It mirrors
// store.ReplayGap, and coredaemon.StreamGap decodes it on the client side.
type apiStreamGap struct {
	// Reason is "too_old" (the ring evicted what the cursor asked for) or
	// "reset" (the cursor is ahead of us — this daemon restarted).
	Reason string `json:"reason"`
	// Since is the cursor the client sent.
	Since uint64 `json:"since"`
	// Oldest is the lowest sequence still replayable (0 when the ring is empty).
	Oldest uint64 `json:"oldest"`
	// Current is the daemon's sequence when the gap was detected.
	Current uint64 `json:"current"`
	// RingSize is the ring bound, so the client can size its own reconnect
	// budget rather than hardcoding ours.
	RingSize int `json:"ring_size"`
}

// typeFilter is a parsed ?types= value: a set of globs matched against the
// wire update_type. A nil/empty filter matches everything.
type typeFilter []string

// parseTypeFilter splits a comma-separated glob list and validates each entry.
// An all-empty value (?types= or ?types=,,) yields the everything filter, so a
// client that builds the query string mechanically never accidentally mutes
// itself.
func parseTypeFilter(raw string) (typeFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out typeFilter
	for _, part := range strings.Split(raw, ",") {
		pattern := strings.TrimSpace(part)
		if pattern == "" {
			continue
		}
		// path.Match compiles lazily, so probe it once to reject a bad glob at
		// request time rather than silently never matching.
		if _, err := path.Match(pattern, "probe"); err != nil {
			return nil, err
		}
		out = append(out, pattern)
	}
	return out, nil
}

// matches reports whether an update_type passes the filter. Glob syntax is
// path.Match's: '*' matches any run of non-'/' characters, which is every
// update type grove has (they are lowercase identifiers), so '*' behaves as
// the intuitive "any suffix" wildcard in job_*.
func (f typeFilter) matches(updateType string) bool {
	if len(f) == 0 {
		return true
	}
	for _, pattern := range f {
		if ok, err := path.Match(pattern, updateType); err == nil && ok {
			return true
		}
	}
	return false
}

// apiUpdateSkipList names store update types that are DELIBERATELY absent from
// the SSE wire, with the reason. Silent drops are hostile to extension authors
// — someone wiring a hook to "test_report" deserves to learn from the code (and
// from a log line) that it never reaches SSE, rather than from an afternoon of
// debugging. Anything NOT in this list and NOT handled by convertUpdatePayload
// is a bug, and noteUnconvertedUpdate says so at WARN.
//
// Adding a case to convertUpdatePayload for one of these is fine; just delete
// its entry here at the same time.
var apiUpdateSkipList = map[store.UpdateType]string{
	store.UpdateJobsDiscovered: "bulk filesystem discovery, not a lifecycle transition; " +
		"consumers reconcile via GET /api/jobs",
	store.UpdateSessionChannels: "in-place session field write; surfaced by the next " +
		"\"session\" frame and GET /api/sessions",
	store.UpdateSessionAutonomous: "in-place session field write; surfaced by the next " +
		"\"session\" frame and GET /api/sessions",
	store.UpdateSessionPing: "idle-ping bookkeeping; too chatty for the wire and carries " +
		"no state a consumer can act on",
	store.UpdateSessionTmuxTarget: "in-place session field write; surfaced by the next " +
		"\"session\" frame and GET /api/sessions",
	store.UpdateSessionLastSender: "in-place session field write; Signal routing detail, " +
		"not a lifecycle event",
	store.UpdatePlans: "full plan-list replacement keyed by plansDir — too large for a " +
		"broadcast; consumers use GET /api/plans",
	store.UpdatePlanIndexSnapshot: "full portfolio snapshot; rides the \"initial\" frame " +
		"(PlanIndexSnapshot) and GET /api/plans/index. Revisioned DELTAS do reach the " +
		"wire as \"plan_index\"",
	store.UpdateMemoryReindex: "internal work trigger for the memory watcher, not a state " +
		"change; the resulting mutations broadcast as \"memory_index\"",
	store.UpdateTestReport: "writes EnrichedWorkspace.TestReports in place; surfaced by the " +
		"next workspace frame. UpdateTaskResult is the type that DOES synthesize a delta",
}

// unconvertedTypeSeen rate-limits noteUnconvertedUpdate to one log line per
// update type per process. The set of types is fixed at compile time, so a
// per-type latch is the tightest useful bound: the operator learns about the
// hole once, and a busy daemon does not turn a missing switch case into a
// gigabyte of logs.
var unconvertedTypeSeen sync.Map // map[store.UpdateType]struct{}

// noteUnconvertedUpdate logs an update that convertUpdatePayload declined to
// put on the wire. Types in apiUpdateSkipList are intentional and log at
// DEBUG; anything else is a missing case and logs at WARN.
func noteUnconvertedUpdate(u store.Update) {
	if _, seen := unconvertedTypeSeen.LoadOrStore(u.Type, struct{}{}); seen {
		return
	}
	ctx := context.Background()
	if reason, intentional := apiUpdateSkipList[u.Type]; intentional {
		streamLog().Debug("Store update intentionally not forwarded to SSE").
			Field("update_type", string(u.Type)).
			Field("reason", reason).
			Log(ctx)
		return
	}
	streamLog().Warn("Store update has no SSE wire mapping and was dropped").
		Field("update_type", string(u.Type)).
		Field("source", u.Source).
		Field("hint", "add a case to convertUpdatePayload, or declare the omission in apiUpdateSkipList").
		Log(ctx)
}

// parseSinceCursor reads the ?since= parameter. The bool reports whether the
// client asked to resume at all, which is distinct from since==0 ("replay
// everything you still hold").
func parseSinceCursor(raw string) (uint64, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}
