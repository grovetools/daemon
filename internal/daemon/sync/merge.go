package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Frontmatter represents parsed YAML frontmatter as key-value pairs.
type Frontmatter map[string]interface{}

// parseFrontmatter extracts frontmatter from document content.
// Expects --- markers and YAML-like key:value pairs.
func parseFrontmatter(content []byte) Frontmatter {
	fm := Frontmatter{}
	lines := strings.Split(string(content), "\n")

	if len(lines) < 2 || lines[0] != "---" {
		return fm
	}

	// Find closing ---
	var endIdx int
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			endIdx = i
			break
		}
	}
	if endIdx == 0 {
		return fm
	}

	// Parse key:value pairs (simplified)
	for i := 1; i < endIdx; i++ {
		parts := strings.SplitN(lines[i], ":", 2)
		if len(parts) == 2 {
			fm[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return fm
}

// frontmatterKeys returns the frontmatter keys of content in file order.
// Used as the ordering hint for reconstructDocument so merges preserve the
// local file's layout instead of leaking map iteration order into bytes.
func frontmatterKeys(content []byte) []string {
	lines := strings.Split(string(content), "\n")
	if len(lines) < 2 || lines[0] != "---" {
		return nil
	}
	var keys []string
	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			return keys
		}
		parts := strings.SplitN(lines[i], ":", 2)
		if len(parts) == 2 {
			keys = append(keys, strings.TrimSpace(parts[0]))
		}
	}
	return nil // no closing marker: parseFrontmatter treats this as no frontmatter
}

// extractBody returns the content after the closing --- marker.
func extractBody(content []byte) []byte {
	lines := strings.Split(string(content), "\n")
	if len(lines) < 2 || lines[0] != "---" {
		return content
	}

	for i := 1; i < len(lines); i++ {
		if lines[i] == "---" {
			return []byte(strings.Join(lines[i+1:], "\n"))
		}
	}
	return content
}

// reconstructDocument builds a document from merged frontmatter and body.
// Keys are emitted following the order hint first (typically
// frontmatterKeys(localContent), preserving the on-disk layout), then any
// remaining keys sorted — output must be deterministic, otherwise repeated
// merges of identical inputs churn content hashes.
func reconstructDocument(frontmatter Frontmatter, order []string, body []byte) []byte {
	if len(frontmatter) == 0 {
		return body
	}

	var result strings.Builder
	result.WriteString("---\n")
	seen := make(map[string]bool, len(frontmatter))
	for _, key := range order {
		if val, ok := frontmatter[key]; ok && !seen[key] {
			result.WriteString(fmt.Sprintf("%s: %v\n", key, val))
			seen[key] = true
		}
	}
	var rest []string
	for key := range frontmatter {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		result.WriteString(fmt.Sprintf("%s: %v\n", key, frontmatter[key]))
	}
	result.WriteString("---\n")
	result.Write(body)
	return []byte(result.String())
}

// modifiedLayouts are the timestamp shapes accepted for the frontmatter
// `modified:` field (nb writes "2006-01-02 15:04:05"; the rest are tolerant
// fallbacks).
var modifiedLayouts = []string{
	"2006-01-02 15:04:05",
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseModifiedTime parses a frontmatter `modified:` value, tolerating
// surrounding quotes. Returns false when missing or unparseable.
func parseModifiedTime(v interface{}) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range modifiedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mergeValues performs a per-key 3-way merge of frontmatter. Frontmatter has
// LWW-map semantics by design: a both-changed key NEVER parks the document —
// every key resolves deterministically (most practical "conflicts" here are
// `modified:` timestamp collisions that deserve auto-resolution).
//
// Rules per key:
//   - only one side changed it → that change is taken (including deletion);
//   - both sides changed it identically → taken once;
//   - both sides changed it differently → the side whose document-level
//     `modified:` timestamp parses LATER wins; if either side's `modified:`
//     is missing/unparseable, or they are equal, LOCAL wins (local is the
//     content on disk and the content we are about to push).
func mergeValues(base, local, remote Frontmatter) Frontmatter {
	merged := Frontmatter{}

	// Start with all keys from all three versions
	allKeys := make(map[string]bool)
	for k := range base {
		allKeys[k] = true
	}
	for k := range local {
		allKeys[k] = true
	}
	for k := range remote {
		allKeys[k] = true
	}

	// Both-changed tiebreak: decided once at document level from `modified:`.
	remoteWins := false
	if lt, lok := parseModifiedTime(local["modified"]); lok {
		if rt, rok := parseModifiedTime(remote["modified"]); rok && rt.After(lt) {
			remoteWins = true
		}
	}

	set := func(key string, val interface{}) {
		// A nil value means the winning side deleted the key: drop it.
		if val != nil {
			merged[key] = val
		}
	}

	for key := range allKeys {
		baseVal := base[key]
		localVal := local[key]
		remoteVal := remote[key]

		// If remote didn't change, keep local (or base if local didn't change)
		if equal(remoteVal, baseVal) {
			set(key, localVal)
			continue
		}

		// If local didn't change, take remote
		if equal(localVal, baseVal) {
			set(key, remoteVal)
			continue
		}

		// Both changed differently: LWW via `modified:`, local on ties/doubt.
		if remoteWins {
			set(key, remoteVal)
		} else {
			set(key, localVal)
		}
	}

	return merged
}

// equal compares two values for equality (simplified).
func equal(a, b interface{}) bool {
	as := fmt.Sprintf("%v", a)
	bs := fmt.Sprintf("%v", b)
	return as == bs
}

// bytesEqual returns true if two byte slices are equal.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// hashContent returns the SHA-256 hash of content as a hex string.
func hashContent(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// emptyContentHash is the SHA-256 of zero bytes — the content_hash carried by
// a legitimately empty document. Pull-side blob-tier detection must treat it
// as inline-empty, never as "content elided, fetch the blob": no blob exists
// for zero bytes.
const emptyContentHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// readFile reads a file from disk.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// writeFile writes content to a file, creating directories as needed. A
// non-zero mtime is restored onto the written file via os.Chtimes — replica
// fidelity for the origin's file timestamp (agents' ls/find heuristics, nb's
// frontmatter-less fallback). A zero mtime (old server/client, or content
// that never had a source file, e.g. conflict artifacts and merged bytes)
// keeps the write time, exactly today's behavior. A Chtimes failure is
// swallowed: mtime is fidelity metadata only and must never fail an apply
// whose content write already succeeded.
func writeFile(path string, content []byte, mtime time.Time) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return err
	}
	if !mtime.IsZero() {
		_ = os.Chtimes(path, mtime, mtime)
	}
	return nil
}

// statMtime returns a path's modification time, or the zero time when the
// stat fails (missing file, permission race). Capture sites use it so an
// mtime lookup can never turn into an error: zero simply means "unknown".
func statMtime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// moveFile renames a file from src to dst.
func moveFile(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// deleteFile removes a file.
func deleteFile(path string) error {
	return os.Remove(path)
}

// deleteDir removes a directory and all its contents.
func deleteDir(path string) error {
	return os.RemoveAll(path)
}
