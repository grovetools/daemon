package telemetry

import (
	"fmt"
	"time"
)

// This file is the single place the daemon's counter NAMES are declared, and
// the only place hot paths need to know about. Recording sites call the
// Record* helpers below; the handles are resolved once at package init so no
// hot path ever takes the registry lock or hashes a string.
//
// Names are dotted lowercase. Stats fan out to <name>.count/.last_ms/
// .mean_ms/.max_ms; rate counters additionally publish <name>_per_min.
// They land verbatim as keys of SystemStats.Counters, so treat a rename as a
// wire-contract change.

// Warning condition strings. Kept as constants because a warning's identity
// is (path, condition) — a typo'd condition string would silently create a
// second, never-clearing warning instead of refreshing the first.
const (
	CondSlowGitSweep     = "slow git status sweep"
	CondSlowGitTrickle   = "slow git status sweep (trickle)"
	CondSlowGitScan      = "slow git watcher scan"
	CondNoopStorm        = "repeated no-op git scans"
	CondLargeBlobHash    = "large file hashed on every scan"
	CondHeapNearLimit    = "heap approaching GOMEMLIMIT"
	CondWatcherStorm     = "filesystem event storm"
	CondDaemonProcStorm  = "daemon subprocess storm"
	CondEventsDropped    = "filesystem events dropped by the kernel"
	CondBudgetExceededFn = "budget exceeded: %s"
)

// Thresholds for the counter-derived warning rules.
const (
	// slowSweepMS mirrors the collector's existing "Slow git status scan
	// detected" log threshold so the log line and the warning agree. It now
	// measures the HOT TIER's completion time, not the whole sweep's: since
	// the sweep became tier-ordered and paced, total wall time is a policy
	// choice (minutes, on purpose) and only the hot tier is a latency anyone
	// experiences. Budget is a little above the ~2s hot-tier target so a
	// normal boot does not flap the alarm.
	slowSweepMS = 2500
	// slowTrickleWorkspaceMS is the trickle's own bar, and it is deliberately
	// NOT a wall-clock measure: the cold tail is slow by design, so the alarm
	// watches the mean COST of one workspace's git calls (summed per
	// workspace, so neither worker count nor pacing sleeps can move it).
	//
	// Anchored on measurements, in summed-cost terms:
	//   ~100 ms  scratch fleet of 650 repos, warm cache
	//   ~335 ms  the healthy 08-01 boot sweep (24s × 8 workers / 573 ws)
	//   ~570 ms  the CONTENDED 08-10 boot sweep this design set out to fix
	//            (48.4s × 8 / 681) — slow from load and volume, not breakage,
	//            and precisely the case that must not raise an alarm now
	//   ~1400 ms the git-scan storms this alarm exists to catch (job 51's
	//            watcher-scan mean over a single repo)
	// So the bar sits between the worst healthy observation and the storm
	// floor, nearer the former because the mean is taken over hundreds of
	// workspaces and a real storm moves all of them.
	slowTrickleWorkspaceMS = 1000
	// slowScanMS is the watcher-side equivalent; the watcher scans ONE
	// workspace, so its bar is lower than a whole-fleet sweep's.
	slowScanMS = 750
	// largeBlobBytes is the size at which hashing a file on every scan is
	// worth telling the user about. The incident that motivated doc 50 was a
	// 60 GB sparse VM disk; 64 MB catches the whole class long before that.
	largeBlobBytes = 64 << 20
	// watcherStormPerMin is a sustained filesystem event rate that means
	// something is writing continuously inside a watched tree.
	watcherStormPerMin = 20000
)

// gitWatcherNoops attributes no-op watcher scans to the repository causing
// them. See noop_storm.go for why this is a bounded side table feeding the
// WarningLedger rather than a per-repo counter.
var gitWatcherNoops = newNoopStormTracker()

var (
	// Git status sweeps (collector: whole in-scope workspace set).
	//
	// git.sweep measures WORK time — the summed scan time of the sweep's
	// batches, pacing sleeps excluded — so its mean stays comparable across
	// the move to a paced sweep. Wall time (which now runs to minutes on
	// purpose) is git.sweep.wall_ms, and the two diverging is the design
	// working, not a stall.
	GitSweep           = Default().Stat("git.sweep")
	GitSweepWorkspaces = Default().Counter("git.sweep.workspaces")
	gitSweepLastCount  = Default().Gauge("git.sweep.workspaces_last")
	gitSweepWallMS     = Default().Gauge("git.sweep.wall_ms")
	// Hot-tier completion: the part of a sweep a user waits on. This is what
	// the re-tuned slow-sweep warning fires on.
	GitSweepHot = Default().Stat("git.sweep.hot")
	// Per-workspace git cost inside sweeps, summed per workspace rather than
	// measured as wall time, so it is independent of worker count and pacing.
	GitSweepWorkspaceCost = Default().Stat("git.sweep.workspace")
	// Live position of the running sweep, for progress rendering without
	// polling the event stream: tier code (0 idle, 1 hot, 2 active, 3 warm,
	// 4 cold), overall done/total, and percent complete.
	gitSweepTier      = Default().Gauge("git.sweep.tier")
	gitSweepDone      = Default().Gauge("git.sweep.done")
	gitSweepTotal     = Default().Gauge("git.sweep.total")
	gitSweepProgress  = Default().Gauge("git.sweep.progress")
	gitSweepTierDone  = Default().Gauge("git.sweep.tier_done")
	gitSweepTierTotal = Default().Gauge("git.sweep.tier_total")
	// Trickle throughput (workspaces per minute of paced-tier wall time) and
	// the fleet-wide honesty gauge: how many in-scope workspaces have never
	// been swept this daemon lifetime. A consumer aggregating "N dirty repos"
	// across the fleet must read this before believing the number.
	gitSweepTricklePerMin = Default().Gauge("git.sweep.trickle_per_min")
	gitSweepPending       = Default().Gauge("git.sweep.pending")

	// Git watcher scans (event-driven, one workspace each).
	GitWatcherScan   = Default().Stat("git.watcher_scan")
	GitWatcherNoop   = Default().Counter("git.watcher_scan.noop")
	GitWatcherEmit   = Default().Counter("git.watcher_scan.emitted")
	GitWatcherFailed = Default().Counter("git.watcher_scan.failed")
	// GitWatcherCoalesced counts scan requests that landed on a workspace whose
	// scan was already running and were folded into its single trailing rerun
	// instead of starting a concurrent one. It is deliberately ONE global
	// counter, not one per repo: Registry.Counter get-or-creates with no cap or
	// eviction and Snapshot() serializes every key into every /api/system/stats
	// response, so 479 repos would permanently inflate the wire payload. The
	// bounded per-repo door is WarningLedger.
	GitWatcherCoalesced = Default().Counter("git.watcher_scan.coalesced")

	// Blob hashing (the hash-object storm's signature).
	BlobHashBatches = Default().Counter("git.blob_hash.batches")
	BlobHashFiles   = Default().Counter("git.blob_hash.files")
	// BlobHashSkipped counts files dropped before hashing: today only
	// nonexistent/unvalidatable paths; once doc 50 Layer 1 lands its size and
	// byte-budget caps, their skips increment this same counter.
	BlobHashSkipped    = Default().Counter("git.blob_hash.skipped")
	BlobHashNonRegular = Default().Counter("git.blob_hash.nonregular")
	BlobHashBatch      = Default().Stat("git.blob_hash")
	blobHashLargest    = Default().Gauge("git.blob_hash.largest_offender_bytes")

	// Filesystem watcher ingest/filter.
	WatcherEventsRaw     = Default().RateCounter("watcher.events.raw")
	WatcherEventsMatched = Default().RateCounter("watcher.events.matched")
	WatcherEventsDropped = Default().Counter("watcher.events.dropped")
	WatcherBatches       = Default().Counter("watcher.batches")
	// WatcherEventsSuppressed counts routed events dropped by dead-subtree
	// suppression: the path lay under a directory git itself proved both ignored
	// and free of tracked files, so a status scan could not have observed
	// anything. Read against watcher.events.matched — the difference is the
	// scans this fix removed — and against git.watcher_scan.emitted, which must
	// NOT fall.
	WatcherEventsSuppressed = Default().Counter("watcher.events.suppressed")

	// SSE fan-out (/api/stream). Counted per subscriber-event, not per store
	// broadcast: the cost this measures is serialize-and-write, which the
	// daemon pays once per subscriber, and the whole point of subscribe-time
	// filtering is to not pay it. published == delivered + filtered by
	// construction — an event that never survives conversion to the public
	// wire shape is counted by neither.
	SSEEventsPublished = Default().Counter("sse.events.published")
	SSEEventsDelivered = Default().Counter("sse.events.delivered")
	SSEEventsFiltered  = Default().Counter("sse.events.filtered")
	// The subscribe-time snapshot is tracked separately because it is not one
	// event among many: it is the whole enriched-workspace map, and on a mature
	// host it dwarfs every other frame combined. sse.initial.skipped is
	// therefore the counter that shows whether filtering is actually paying.
	SSEInitialSent    = Default().Counter("sse.initial.sent")
	SSEInitialSkipped = Default().Counter("sse.initial.skipped")
	// Marshal-once frame cache. One publish reaches every subscriber's request
	// goroutine, and each used to convert and marshal the SAME update
	// independently — N copies of byte-identical JSON per event, which at a
	// mature host's frame sizes was the largest single allocator on the stream
	// path. The cache keys that work by store sequence, so hits ≈
	// (subscribers − 1) × events for the unfiltered majority.
	//
	// misses is the number of marshals actually performed, so
	// hits/(hits+misses) is the fan-out saving directly. shared_bytes is the
	// JSON that was NOT produced a second time — the allocation this removes.
	SSEMarshalCacheHits   = Default().Counter("sse.marshal.cache_hits")
	SSEMarshalCacheMisses = Default().Counter("sse.marshal.cache_misses")
	SSEMarshalSharedBytes = Default().Counter("sse.marshal.shared_bytes")
	// The subscribe-time snapshot gets its own cache (keyed by sequence AND
	// filter, TTL'd) because it is the one frame big enough that a reconnect
	// storm marshalling it once per client is visible in a heap profile.
	SSEInitialCacheHits   = Default().Counter("sse.initial.cache_hits")
	SSEInitialCacheMisses = Default().Counter("sse.initial.cache_misses")
	// SSEFrameCacheEvicted counts frames dropped from the ring by the byte
	// budget rather than by sequence recycling. A nonzero, growing value means
	// frames are large enough that the budget — not the slot count — is what
	// bounds the cache, which is the signal to re-read the frame sizes.
	SSEFrameCacheEvicted = Default().Counter("sse.marshal.cache_evicted")

	// Note-index publish fence. Both the note collector and the note watcher
	// rebuild the WHOLE index (tens of thousands of entries at ecosystem
	// scale) and used to publish it unconditionally, so every scan handed the
	// 1024-slot replay ring another full map to retain even when not one entry
	// had changed. published/suppressed is how often that map actually moved.
	NoteIndexPublished  = Default().Counter("note.index.published")
	NoteIndexSuppressed = Default().Counter("note.index.suppressed")
	noteIndexEntries    = Default().Gauge("note.index.entries")

	// Aggregated plan-stats enrichment: one pass recounts every workspace's
	// plan/job totals. It is kicked by plan-file events on a short debounce and
	// used to be invisible — only the synchronous plan-index refresh it hangs
	// off was logged — so a pass that had grown to seconds could run every few
	// seconds without leaving a number anywhere. These are that number.
	PlanStatsPass           = Default().Stat("planstats.pass")
	PlanStatsWorkspaces     = Default().Counter("planstats.pass.workspaces")
	planStatsWorkspacesLast = Default().Gauge("planstats.pass.workspaces_last")
	// Event triage in FRONT of that pass. A plans directory is written into
	// continuously by whatever agents are running under it — job logs, chat
	// transcripts, `.artifacts/` output — and none of that is readable by the
	// plan index or the stats recount, both of which open a strictly bounded
	// set of files. suppressed counts the events dropped on that ground before
	// they could arm the refresh debounce; read it against
	// planstats.events.kept, whose ratio is how much of the plan-file event
	// stream was pure churn.
	PlanStatsEventsKept       = Default().RateCounter("planstats.events.kept")
	PlanStatsEventsSuppressed = Default().RateCounter("planstats.events.suppressed")
	// PlanStatsDeferred counts passes the rate floor pushed onto a trailing
	// timer rather than running immediately. Every increment is one whole
	// portfolio recount that did not happen, so this is the floor's yield —
	// and because the trailing run always fires, it is a deferral count, never
	// a drop count.
	PlanStatsDeferred = Default().Counter("planstats.pass.deferred")
	// Reruns: a pass that finished and immediately owed another one. This is
	// the treadmill's signature — at 600 workspaces a pass takes seconds, so
	// anything that reliably owes a rerun makes the pass's own duration the
	// reason it runs again. Exactly one cause is attributed per rerun, so
	// planstats.rerun.count == .queued + .seq_race:
	//
	//   - queued: a kick arrived while the pass was running and coalesced into
	//     its trailing run. Legitimate — something asked for a recount.
	//   - seq_race: no kick, but an index publish moved disk state the stats
	//     reader can actually observe while the pass was reading it, so the
	//     pass's answer was already stale on arrival.
	//
	// seq_race used to fire on EVERY index publish, including overlay-only
	// re-projections that touch no plan file; watch it stay near zero.
	PlanStatsRerun        = Default().Counter("planstats.rerun.count")
	PlanStatsRerunQueued  = Default().Counter("planstats.rerun.queued")
	PlanStatsRerunSeqRace = Default().Counter("planstats.rerun.seq_race")

	// Transcript parsing (the rescan-loop signature).
	TranscriptConsidered = Default().RateCounter("transcript.considered")
	TranscriptParsed     = Default().RateCounter("transcript.parses")
	TranscriptUnchanged  = Default().Counter("transcript.unchanged")
	TranscriptParse      = Default().Stat("transcript.parse")

	// Effective (not configured) collector intervals.
	collectorInterval = func(name string) *Gauge {
		return Default().Gauge("collector." + name + ".interval_ms")
	}
)

// RecordGitSweep records one completed collector git-status sweep over n
// workspaces: work is the summed scan time of its batches (pacing sleeps
// excluded) and wall is how long it actually took end to end.
//
// It deliberately raises NO warning. Since the sweep became tier-ordered and
// paced, a long sweep is the policy rather than a symptom — the two questions
// worth alarming on are split out into RecordGitSweepHot (the latency users
// feel) and RecordGitSweepTrickle (whether git itself got slow), so intentional
// slowness cannot fire the alarm and a real regression still does.
func RecordGitSweep(scope string, n int, work, wall time.Duration) {
	if n <= 0 {
		return
	}
	GitSweep.ObserveDuration(work)
	GitSweepWorkspaces.Add(int64(n))
	gitSweepLastCount.Set(float64(n))
	gitSweepWallMS.Set(float64(wall.Milliseconds()))
}

// RecordGitSweepHot records the hot tier's completion: n focused workspaces
// swept within d of the sweep starting. This is the number a user experiences
// as "the daemon caught up with what I am looking at", and the one the
// slow-sweep warning fires on.
func RecordGitSweepHot(scope string, n int, d time.Duration) {
	if n <= 0 {
		return
	}
	GitSweepHot.ObserveDuration(d)
	if d.Milliseconds() >= slowSweepMS {
		Default().Warnings().Raise(
			scopeLabel(scope),
			CondSlowGitSweep,
			fmt.Sprintf("hot tier: %d workspaces in %s", n, d.Round(time.Millisecond)),
		)
	}
}

// RecordGitSweepTrickle records the paced tail's throughput: n workspaces
// scanned for cost of summed per-workspace git time, over elapsed wall time.
//
// The warning is on COST per workspace, never on elapsed: the trickle is slow
// on purpose, and an alarm that could not tell that apart from a storm would
// be turned off within a week. perMin is published as a gauge so the trickle's
// actual throughput stays visible next to the reason it is low.
func RecordGitSweepTrickle(scope string, n int, cost, elapsed time.Duration) {
	if n <= 0 {
		return
	}
	perWorkspace := cost / time.Duration(n)
	GitSweepWorkspaceCost.ObserveDuration(perWorkspace)
	if elapsed > 0 {
		gitSweepTricklePerMin.Set(float64(n) / elapsed.Minutes())
	}
	if perWorkspace.Milliseconds() >= slowTrickleWorkspaceMS {
		Default().Warnings().Raise(
			scopeLabel(scope),
			CondSlowGitTrickle,
			fmt.Sprintf("%s of git per workspace over %d workspaces",
				perWorkspace.Round(time.Millisecond), n),
		)
	}
}

// RecordGitSweepProgress publishes the running sweep's position. tier is the
// numeric tier code (0 == idle, higher == colder); done/total are workspaces.
func RecordGitSweepProgress(tier, tierDone, tierTotal, done, total int) {
	gitSweepTier.Set(float64(tier))
	gitSweepTierDone.Set(float64(tierDone))
	gitSweepTierTotal.Set(float64(tierTotal))
	gitSweepDone.Set(float64(done))
	gitSweepTotal.Set(float64(total))
	if total > 0 {
		gitSweepProgress.Set(float64(done) * 100 / float64(total))
	} else {
		gitSweepProgress.Set(0)
	}
}

// RecordGitSweepIdle marks no sweep in flight, so a stale tier code cannot be
// read as a sweep that never ends.
func RecordGitSweepIdle() {
	gitSweepTier.Set(0)
}

// RecordGitSweepPending publishes how many in-scope workspaces have never been
// swept this daemon lifetime — the fleet-wide counterpart of the per-workspace
// GitStatusPending flag.
func RecordGitSweepPending(n int) {
	gitSweepPending.Set(float64(n))
}

// RecordGitWatcherScan records one event-driven per-workspace scan. The path
// is used for attribution only: a scan slower than slowScanMS names the repo,
// and so does a repo whose no-op rate separates it from the fleet (the whole
// point of a global 99%-no-op ratio is that it cannot tell you WHICH repo).
func RecordGitWatcherScan(path string, d time.Duration, emitted bool) {
	GitWatcherScan.ObserveDuration(d)
	if emitted {
		GitWatcherEmit.Inc()
	} else {
		GitWatcherNoop.Inc()
		recordNoopScan(Default().Warnings(), gitWatcherNoops, path, time.Now())
	}
	if d.Milliseconds() >= slowScanMS {
		Default().Warnings().Raise(path, CondSlowGitScan,
			fmt.Sprintf("scan took %s", d.Round(time.Millisecond)))
	}
}

// BlobHashObservation is what a blob-hash batch reports back. It mirrors
// core/git's BlobHashStats delta so doc 50's future byte-budget skips slot in
// without changing this signature.
type BlobHashObservation struct {
	Repo         string
	Files        int
	Skipped      int
	NonRegular   int
	LargestBytes int64
	LargestPath  string
	Duration     time.Duration
}

// RecordBlobHash records one `git hash-object --stdin-paths` batch and raises
// the doc-50 Layer-2 alert when a single huge file is being rehashed on every
// scan of a repo.
func RecordBlobHash(o BlobHashObservation) {
	BlobHashBatches.Inc()
	BlobHashFiles.Add(int64(o.Files))
	BlobHashSkipped.Add(int64(o.Skipped))
	BlobHashNonRegular.Add(int64(o.NonRegular))
	BlobHashBatch.ObserveDuration(o.Duration)
	if float64(o.LargestBytes) > blobHashLargest.Value() {
		blobHashLargest.Set(float64(o.LargestBytes))
	}
	if o.LargestBytes >= largeBlobBytes && o.LargestPath != "" {
		Default().Warnings().Raise(o.Repo, CondLargeBlobHash,
			fmt.Sprintf("%s (%s) — consider .gitignore", o.LargestPath, humanBytes(o.LargestBytes)))
	}
}

// RecordNoteIndexPublish records the outcome of one note-index publish
// attempt. entries is the size of the index the producer built, recorded on
// both outcomes: a suppressed publish still proves the index was rebuilt, and
// the gauge is how an operator sizes what the fence is holding back.
func RecordNoteIndexPublish(entries int, published bool) {
	if published {
		NoteIndexPublished.Inc()
	} else {
		NoteIndexSuppressed.Inc()
	}
	noteIndexEntries.Set(float64(entries))
}

// RecordPlanStatsPass records one aggregated PlanStats pass over n workspaces.
// n is recorded even for an empty pass (unlike RecordGitSweep's n<=0 guard):
// "the pass ran and saw nothing" is itself a diagnosis, and dropping those
// samples would make mean_ms report only the expensive passes.
func RecordPlanStatsPass(n int, d time.Duration) {
	PlanStatsPass.ObserveDuration(d)
	PlanStatsWorkspaces.Add(int64(n))
	planStatsWorkspacesLast.Set(float64(n))
}

// RecordPlanStatsEvents records one classified batch of plan-directory events:
// kept are the ones something the flow watcher publishes can actually read,
// suppressed the ones provably invisible to both readers.
func RecordPlanStatsEvents(kept, suppressed int) {
	if kept > 0 {
		PlanStatsEventsKept.Add(int64(kept))
	}
	if suppressed > 0 {
		PlanStatsEventsSuppressed.Add(int64(suppressed))
	}
}

// RecordPlanStatsDeferred records one aggregated-PlanStats pass held back by
// the rate floor and handed to its trailing timer instead.
func RecordPlanStatsDeferred() { PlanStatsDeferred.Inc() }

// RecordPlanStatsRerun records one pass that owed another as soon as it
// finished. queued distinguishes the two causes; when both hold, the explicit
// kick is the one attributed, which keeps the total the sum of its parts.
func RecordPlanStatsRerun(queued bool) {
	PlanStatsRerun.Inc()
	if queued {
		PlanStatsRerunQueued.Inc()
		return
	}
	PlanStatsRerunSeqRace.Inc()
}

// RecordWatcherBatch records one debounce batch from the unified watcher.
func RecordWatcherBatch(raw int) {
	WatcherBatches.Inc()
	WatcherEventsRaw.Add(int64(raw))
}

// RecordWatcherMatched records events that survived a handler's filter.
func RecordWatcherMatched(n int) { WatcherEventsMatched.Add(int64(n)) }

// RecordWatcherSuppressed records one event dropped as provably invisible to
// git.
func RecordWatcherSuppressed() { WatcherEventsSuppressed.Inc() }

// RecordWatcherDropped records kernel-coalesced/dropped FSEvents batches,
// which force a fan-out rescan of every route and are therefore a first-class
// cause of surprise sweep load.
func RecordWatcherDropped(routes int) {
	WatcherEventsDropped.Inc()
	Default().Warnings().Raise("fsevents", CondEventsDropped,
		fmt.Sprintf("kernel drop forced a rescan of %d routes", routes))
}

// RecordTranscriptParse records one live-session token refresh: considered is
// always incremented, parsed only when the transcript's mtime actually moved.
func RecordTranscriptParse(parsed bool, d time.Duration) {
	TranscriptConsidered.Inc()
	if parsed {
		TranscriptParsed.Inc()
		TranscriptParse.ObserveDuration(d)
	} else {
		TranscriptUnchanged.Inc()
	}
}

// SetCollectorInterval publishes a collector's EFFECTIVE interval — the one
// its ticker is actually running at after dynamic adjustment, not the value
// it was configured with. The distinction is the whole point of the counter.
func SetCollectorInterval(name string, d time.Duration) {
	collectorInterval(name).Set(float64(d.Milliseconds()))
}

// CheckWatcherStorm raises the event-storm warning when the ingest rate is
// sustained above watcherStormPerMin. Called from the snapshot path rather
// than the ingest path so the check itself costs nothing per event.
func CheckWatcherStorm() {
	if rate := WatcherEventsRaw.PerMin(); rate >= watcherStormPerMin {
		Default().Warnings().Raise("fsevents", CondWatcherStorm,
			fmt.Sprintf("%.0f events/min ingested", rate))
	}
}

func scopeLabel(scope string) string {
	if scope == "" {
		return "(global)"
	}
	return scope
}

// humanBytes renders a byte count the way the CLI and TUI do.
func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
