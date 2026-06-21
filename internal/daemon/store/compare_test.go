package store

import (
	"testing"

	"github.com/grovetools/core/git"
)

// TestGitStatusEqual_AheadMain verifies that AheadMainCount, BehindMainCount,
// and HasUpstream participate in the equality check. These fields drive the
// sessionizer GIT column, and omitting them previously swallowed
// ahead-of-local-main changes (e.g. a commit to a branch) so no delta was
// broadcast.
func TestGitStatusEqual_AheadMain(t *testing.T) {
	base := func() *git.ExtendedGitStatus {
		return &git.ExtendedGitStatus{
			StatusInfo: &git.StatusInfo{
				Branch:          "feature",
				AheadCount:      0,
				BehindCount:     0,
				ModifiedCount:   0,
				UntrackedCount:  0,
				StagedCount:     0,
				IsDirty:         false,
				HasUpstream:     false,
				AheadMainCount:  0,
				BehindMainCount: 0,
			},
		}
	}

	t.Run("identical statuses are equal", func(t *testing.T) {
		if !GitStatusEqual(base(), base()) {
			t.Fatal("expected identical statuses to be equal")
		}
	})

	t.Run("differing AheadMainCount is not equal", func(t *testing.T) {
		a := base()
		b := base()
		b.StatusInfo.AheadMainCount = 1
		if GitStatusEqual(a, b) {
			t.Fatal("expected statuses differing only in AheadMainCount to be unequal")
		}
	})

	t.Run("differing BehindMainCount is not equal", func(t *testing.T) {
		a := base()
		b := base()
		b.StatusInfo.BehindMainCount = 2
		if GitStatusEqual(a, b) {
			t.Fatal("expected statuses differing only in BehindMainCount to be unequal")
		}
	})

	t.Run("differing HasUpstream is not equal", func(t *testing.T) {
		a := base()
		b := base()
		b.StatusInfo.HasUpstream = true
		if GitStatusEqual(a, b) {
			t.Fatal("expected statuses differing only in HasUpstream to be unequal")
		}
	})
}
