package store

import (
	"github.com/grovetools/core/git"
	"github.com/grovetools/core/pkg/models"
)

// GitStatusEqual returns true if two ExtendedGitStatus values are equivalent.
// Used to suppress no-op updates that would cause unnecessary TUI re-renders.
func GitStatusEqual(a, b *git.ExtendedGitStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.LinesAdded != b.LinesAdded || a.LinesDeleted != b.LinesDeleted {
		return false
	}
	if a.StatusInfo == nil && b.StatusInfo == nil {
		return true
	}
	if a.StatusInfo == nil || b.StatusInfo == nil {
		return false
	}
	sa, sb := a.StatusInfo, b.StatusInfo
	return sa.Branch == sb.Branch &&
		sa.AheadCount == sb.AheadCount &&
		sa.BehindCount == sb.BehindCount &&
		sa.ModifiedCount == sb.ModifiedCount &&
		sa.UntrackedCount == sb.UntrackedCount &&
		sa.StagedCount == sb.StagedCount &&
		sa.IsDirty == sb.IsDirty &&
		sa.AheadMainCount == sb.AheadMainCount &&
		sa.BehindMainCount == sb.BehindMainCount &&
		sa.HasUpstream == sb.HasUpstream
}

// FileDataEqual returns true if two per-file change snapshots (ChangedFiles +
// BlobHashes) are equivalent. GitStatusEqual only compares coarse status, so an
// edit to an already-modified file (numstat/blob content moved, counts didn't)
// is invisible to it — the git delta emitters compare per-file data with this
// to avoid suppressing such content-only changes for focused repos. Order-
// sensitive on the file list: GetChangedFiles output is stable, and a spurious
// mismatch just emits one extra delta.
func FileDataEqual(aFiles []git.FileStatus, aHashes map[string]string, bFiles []git.FileStatus, bHashes map[string]string) bool {
	if len(aFiles) != len(bFiles) || len(aHashes) != len(bHashes) {
		return false
	}
	for i := range aFiles {
		if aFiles[i] != bFiles[i] {
			return false
		}
	}
	for k, v := range aHashes {
		if bHashes[k] != v {
			return false
		}
	}
	return true
}

// PlanStatsEqual returns true if two PlanStats values are equivalent.
func PlanStatsEqual(a, b *models.PlanStats) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.TotalPlans == b.TotalPlans &&
		a.ActivePlan == b.ActivePlan &&
		a.Running == b.Running &&
		a.Pending == b.Pending &&
		a.Completed == b.Completed &&
		a.Failed == b.Failed &&
		a.Todo == b.Todo &&
		a.Hold == b.Hold &&
		a.Abandoned == b.Abandoned &&
		a.PlanStatus == b.PlanStatus &&
		a.AssociatedPlan == b.AssociatedPlan &&
		a.AssociatedPlanDir == b.AssociatedPlanDir
}

// NoteCountsEqual returns true if two NoteCounts values are equivalent.
func NoteCountsEqual(a, b *models.NoteCounts) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Current == b.Current &&
		a.Issues == b.Issues &&
		a.Inbox == b.Inbox &&
		a.Docs == b.Docs &&
		a.Completed == b.Completed &&
		a.Review == b.Review &&
		a.InProgress == b.InProgress &&
		a.Other == b.Other
}
