package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// reconstructDocument builds a new document from merged frontmatter and remote body.
func reconstructDocument(frontmatter Frontmatter, body []byte) []byte {
	if len(frontmatter) == 0 {
		return body
	}

	var result strings.Builder
	result.WriteString("---\n")
	for key, val := range frontmatter {
		result.WriteString(fmt.Sprintf("%s: %v\n", key, val))
	}
	result.WriteString("---\n")
	result.Write(body)
	return []byte(result.String())
}

// mergeValues performs field-level merge of frontmatter.
// If both local and remote changed a field differently, remote wins.
// If only one side changed it, that change is taken.
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

	for key := range allKeys {
		baseVal := base[key]
		localVal := local[key]
		remoteVal := remote[key]

		// If remote didn't change, keep local (or base if local didn't change)
		if equal(remoteVal, baseVal) {
			merged[key] = localVal
			continue
		}

		// If local didn't change, take remote
		if equal(localVal, baseVal) {
			merged[key] = remoteVal
			continue
		}

		// Both changed: remote wins (server-arrival order is canonical)
		merged[key] = remoteVal
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

// readFile reads a file from disk.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// writeFile writes content to a file, creating directories as needed.
func writeFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0644)
}

// moveFile renames a file from src to dst.
func moveFile(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
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
