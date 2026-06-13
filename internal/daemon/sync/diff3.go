package sync

// diff3.go implements a line-based three-way merge for document bodies (the
// universal floor of the conflict-resolution posture: diff3 now, CRDTs as a
// capabilities-negotiated vNext doc type — see the sync-conflict-resolution
// concept note). Pure functions, no third-party dependencies, deliberately
// boring: base→local and base→remote line diffs are computed via LCS, the
// resulting hunks are composed when they touch disjoint base regions, and any
// genuinely overlapping change (including competing insertions at the same
// point, e.g. both sides appending at end-of-file) is a conflict. A visible
// parked conflict is more honest than an invisible bad merge.

import (
	"bytes"
	"strings"
)

// maxDiffCells caps the LCS dynamic-programming table size (cells =
// base-lines × other-lines after common prefix/suffix trimming). Beyond the
// cap diffHunks degrades to a single coarse hunk covering the whole trimmed
// region — still a valid edit script, just maximally conservative: coarser
// hunks can only produce MORE conflicts, never a wrong merge.
const maxDiffCells = 4 << 20

// hunk is one contiguous change against the base: base[start:end) is replaced
// by lines. start == end is a pure insertion at that base position.
type hunk struct {
	start, end int
	lines      []string
}

// splitLines splits content into lines, each retaining its trailing '\n'
// (except a final unterminated line). This keeps trailing-newline differences
// first-class: "a\nb" and "a\nb\n" differ in their last line.
func splitLines(b []byte) []string {
	var lines []string
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			lines = append(lines, string(b))
			break
		}
		lines = append(lines, string(b[:i+1]))
		b = b[i+1:]
	}
	return lines
}

// diffHunks computes the minimal (LCS-based) line edit script from base to
// other as a sorted list of non-adjacent hunks. Consecutive hunks are always
// separated by at least one matched base line.
func diffHunks(base, other []string) []hunk {
	// Trim common prefix.
	p := 0
	for p < len(base) && p < len(other) && base[p] == other[p] {
		p++
	}
	// Trim common suffix (without crossing the prefix).
	s := 0
	for s < len(base)-p && s < len(other)-p && base[len(base)-1-s] == other[len(other)-1-s] {
		s++
	}
	mb := base[p : len(base)-s]
	mo := other[p : len(other)-s]
	n, m := len(mb), len(mo)
	if n == 0 && m == 0 {
		return nil
	}

	if n*m > maxDiffCells {
		// Coarse fallback: one hunk replacing the whole trimmed region.
		return []hunk{{start: p, end: len(base) - s, lines: append([]string(nil), mo...)}}
	}

	// lcs[i][j] = LCS length of mb[i:] vs mo[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if mb[i] == mo[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	// Walk the table emitting hunks. Matching equal lines is always optimal
	// for LCS, so a position with equal lines closes the current hunk.
	var hunks []hunk
	var cur *hunk
	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && mb[i] == mo[j] {
			cur = nil
			i++
			j++
			continue
		}
		if cur == nil {
			hunks = append(hunks, hunk{start: p + i, end: p + i})
			cur = &hunks[len(hunks)-1]
		}
		if i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]) {
			cur.end = p + i + 1 // delete base line i
			i++
		} else {
			cur.lines = append(cur.lines, mo[j]) // insert other line j
			j++
		}
	}
	return hunks
}

// hunksEqual reports whether two hunks describe the identical change.
func hunksEqual(a, b hunk) bool {
	if a.start != b.start || a.end != b.end || len(a.lines) != len(b.lines) {
		return false
	}
	for i := range a.lines {
		if a.lines[i] != b.lines[i] {
			return false
		}
	}
	return true
}

// hunksOverlap reports whether two hunks touch the same base region. Two
// zero-width hunks (insertions) at the same position compete for the same
// spot — ordering them would be a guess, so they overlap. A zero-width hunk
// at the boundary of a non-empty region does NOT overlap it (it composes
// before/after the changed region deterministically).
func hunksOverlap(a, b hunk) bool {
	if a.start == a.end && b.start == b.end {
		return a.start == b.start
	}
	return a.start < b.end && b.start < a.end
}

// diff3Merge performs a line-based three-way merge of document bodies.
// It returns (merged, true) when every change composes: hunks present on only
// one side apply as-is, identical hunks on both sides apply once, and
// disjoint hunks from both sides interleave by base position. It returns
// (nil, false) when any local hunk overlaps any remote hunk — both sides
// changed the same base region differently — which is a conflict.
func diff3Merge(base, local, remote []byte) ([]byte, bool) {
	// Fast paths: identical changes, or one side unchanged.
	if bytes.Equal(local, remote) {
		return append([]byte(nil), local...), true
	}
	if bytes.Equal(base, local) {
		return append([]byte(nil), remote...), true
	}
	if bytes.Equal(base, remote) {
		return append([]byte(nil), local...), true
	}

	baseLines := splitLines(base)
	lh := diffHunks(baseLines, splitLines(local))
	rh := diffHunks(baseLines, splitLines(remote))

	var out []string
	pos := 0
	emit := func(h hunk) {
		out = append(out, baseLines[pos:h.start]...)
		out = append(out, h.lines...)
		pos = h.end
	}

	li, ri := 0, 0
	for li < len(lh) || ri < len(rh) {
		switch {
		case ri == len(rh):
			emit(lh[li])
			li++
		case li == len(lh):
			emit(rh[ri])
			ri++
		default:
			a, b := lh[li], rh[ri]
			if hunksEqual(a, b) {
				emit(a)
				li++
				ri++
				continue
			}
			if hunksOverlap(a, b) {
				return nil, false
			}
			// Disjoint: emit the earlier hunk. On a start tie one of them is
			// a pure insertion (same-width ties overlap above); the insertion
			// goes first so it lands before the replaced region.
			if a.start < b.start || (a.start == b.start && a.start == a.end) {
				emit(a)
				li++
			} else {
				emit(b)
				ri++
			}
		}
	}
	out = append(out, baseLines[pos:]...)
	return []byte(strings.Join(out, "")), true
}
