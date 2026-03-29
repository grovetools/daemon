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
		sa.IsDirty == sb.IsDirty
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
		a.PlanStatus == b.PlanStatus
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
