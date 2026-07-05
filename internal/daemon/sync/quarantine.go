package sync

import "regexp"

// secretPatterns are the quarantine heuristics: a document matching any of
// these is never queued to the outbox. Conservative, high-signal patterns
// only — quarantine is the backstop behind nb's Phase 1 frontmatter token
// cleanup, not a general secret scanner.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"github fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)},
	{"github token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`)},
	{"private key block", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"aws access key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"slack token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`)},
	{"openai project key", regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{20,}`)},
	{"openrouter key", regexp.MustCompile(`sk-or-v1-[A-Za-z0-9]{20,}`)},
	{"anthropic key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`)},
}

// ScanForSecrets returns the name of the first matching secret heuristic.
// It is the single quarantine gate shared by every outbox producer: the
// watcher's flush path and the anti-entropy push sweep must apply identical
// rules, or a document quarantined live would leak through reconciliation.
func ScanForSecrets(content []byte) (string, bool) {
	for _, p := range secretPatterns {
		if p.re.Match(content) {
			return p.name, true
		}
	}
	return "", false
}
