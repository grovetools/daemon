// Package gitlimits holds the concurrency bound shared by every path in the
// daemon that forks git.
//
// Two independent pools draw from it: the collector's sweep workers
// (internal/daemon/collector) and the watcher's event-driven scan workers
// (internal/daemon/watcher). They are separate pools of the same size — a
// collector sweep must not be able to starve event-driven freshness, and vice
// versa — but the SIZE lives here so the two cannot drift apart. Before this
// existed the collector was bounded and the watcher was not at all, which is
// how overlapping scans of one repository turned a 6 ms `git status` into a
// 1400 ms mean.
package gitlimits

import "runtime"

// Workers is the maximum number of concurrent git status invocations a single
// pool may run. Half the cores (floor 2, ceiling 8) keeps the daemon
// unobtrusive on a laptop that is also running the agents doing the writing.
var Workers = max(min(runtime.NumCPU()/2, 8), 2)
