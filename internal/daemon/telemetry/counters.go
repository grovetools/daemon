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
	// detected" log threshold so the log line and the warning agree.
	slowSweepMS = 1000
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
	GitSweep           = Default().Stat("git.sweep")
	GitSweepWorkspaces = Default().Counter("git.sweep.workspaces")
	gitSweepLastCount  = Default().Gauge("git.sweep.workspaces_last")

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

	// Aggregated plan-stats enrichment: one pass recounts every workspace's
	// plan/job totals. It is kicked by plan-file events on a short debounce and
	// used to be invisible — only the synchronous plan-index refresh it hangs
	// off was logged — so a pass that had grown to seconds could run every few
	// seconds without leaving a number anywhere. These are that number.
	PlanStatsPass           = Default().Stat("planstats.pass")
	PlanStatsWorkspaces     = Default().Counter("planstats.pass.workspaces")
	planStatsWorkspacesLast = Default().Gauge("planstats.pass.workspaces_last")

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

// RecordGitSweep records one collector git-status sweep over n workspaces.
// A sweep slower than slowSweepMS also raises a health warning against the
// scope, since that is precisely the degradation users otherwise only notice
// as "the TUI feels stale".
func RecordGitSweep(scope string, n int, d time.Duration) {
	if n <= 0 {
		return
	}
	GitSweep.ObserveDuration(d)
	GitSweepWorkspaces.Add(int64(n))
	gitSweepLastCount.Set(float64(n))
	if d.Milliseconds() >= slowSweepMS {
		Default().Warnings().Raise(
			scopeLabel(scope),
			CondSlowGitSweep,
			fmt.Sprintf("%d workspaces in %s", n, d.Round(time.Millisecond)),
		)
	}
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

// RecordPlanStatsPass records one aggregated PlanStats pass over n workspaces.
// n is recorded even for an empty pass (unlike RecordGitSweep's n<=0 guard):
// "the pass ran and saw nothing" is itself a diagnosis, and dropping those
// samples would make mean_ms report only the expensive passes.
func RecordPlanStatsPass(n int, d time.Duration) {
	PlanStatsPass.ObserveDuration(d)
	PlanStatsWorkspaces.Add(int64(n))
	planStatsWorkspacesLast.Set(float64(n))
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
