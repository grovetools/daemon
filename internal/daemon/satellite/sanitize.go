package satellite

import (
	"regexp"
	"strings"

	"github.com/grovetools/core/pkg/models"
)

// escapeSeq matches ANSI escape sequences that a malicious or noisy satellite
// could smuggle into a string field of a JobInfo/Session (M2 contract C9,
// considerations §9): CSI (`ESC [ … final`), OSC (`ESC ] … BEL|ST`), and any
// other two-byte `ESC <char>` escape. Remote state is untrusted input; these
// are all single-line UI fields, so every escape is stripped before the row
// ever reaches the Store — no downstream UI re-sanitizes.
var escapeSeq = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC terminated by BEL or ST
		"|\x1b[@-Z\\\\-_]", // other single-char escapes (incl. lone ESC forms)
)

// stripCtl removes ANSI escape sequences and every remaining C0 control byte
// (0x00–0x1F) and DEL (0x7F) from s. JobInfo/Session string fields are all
// single-line, so newlines/tabs are noise here and are dropped too. This is the
// one sanitization primitive; both SanitizeJobInfo and SanitizeSession use it.
func stripCtl(s string) string {
	if s == "" {
		return s
	}
	s = escapeSeq.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// stripSlice sanitizes every element of a string slice in place, returning it
// for convenience. Nil-safe.
func stripSlice(in []string) []string {
	for i, v := range in {
		in[i] = stripCtl(v)
	}
	return in
}

// stripMap sanitizes both keys and values of a string map, returning a fresh
// map (keys may collapse after stripping; last-writer-wins is fine for these
// display-only env fields). Nil-safe.
func stripMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[stripCtl(k)] = stripCtl(v)
	}
	return out
}

// SanitizeJobInfo scrubs every string field of a remote-sourced JobInfo and
// FORCES Origin to the given registry name last (C6/C9). It mutates j in place
// and returns it. Call it on every row from a satellite BEFORE ApplyUpdate —
// the Origin overwrite is what makes the value spoof-proof (the wire value,
// including empty string, is discarded).
func SanitizeJobInfo(j *models.JobInfo, origin string) *models.JobInfo {
	if j == nil {
		return nil
	}
	j.ID = stripCtl(j.ID)
	j.Title = stripCtl(j.Title)
	j.Type = models.JobType(stripCtl(string(j.Type)))
	j.PlanDir = stripCtl(j.PlanDir)
	j.PlanName = stripCtl(j.PlanName)
	j.JobFile = stripCtl(j.JobFile)
	j.WorkDir = stripCtl(j.WorkDir)
	j.Repo = stripCtl(j.Repo)
	j.Branch = stripCtl(j.Branch)
	j.Status = stripCtl(j.Status)
	j.TimeoutStr = stripCtl(j.TimeoutStr)
	j.AgentTarget = stripCtl(j.AgentTarget)
	j.Error = stripCtl(j.Error)
	j.LogFilePath = stripCtl(j.LogFilePath)
	j.Channels = stripSlice(j.Channels)
	j.Env = stripMap(j.Env)
	// Force the origin last — never trust the wire value (C6).
	j.Origin = origin
	return j
}

// SanitizeSession scrubs every string field of a remote-sourced Session,
// FORCES Origin to the registry name last, and zeroes the local-mux routing
// identifiers (PtyID/TmuxTarget) so no local PTY/tmux write path can ever be
// tricked into targeting a local pane off a satellite row (C9, belt-and-braces
// on top of the server mutation guards). Mutates s in place and returns it.
func SanitizeSession(s *models.Session, origin string) *models.Session {
	if s == nil {
		return nil
	}
	s.ID = stripCtl(s.ID)
	s.Type = stripCtl(s.Type)
	s.Repo = stripCtl(s.Repo)
	s.Branch = stripCtl(s.Branch)
	s.TmuxKey = stripCtl(s.TmuxKey)
	s.WorkingDirectory = stripCtl(s.WorkingDirectory)
	s.User = stripCtl(s.User)
	s.Status = stripCtl(s.Status)
	s.PlanName = stripCtl(s.PlanName)
	s.PlanDirectory = stripCtl(s.PlanDirectory)
	s.JobTitle = stripCtl(s.JobTitle)
	s.JobFilePath = stripCtl(s.JobFilePath)
	s.ParentJobID = stripCtl(s.ParentJobID)
	s.ClaudeSessionID = stripCtl(s.ClaudeSessionID)
	s.Provider = stripCtl(s.Provider)
	s.TranscriptPath = stripCtl(s.TranscriptPath)
	s.Model = stripCtl(s.Model)
	s.Mux = stripCtl(s.Mux)
	s.LastSender = stripCtl(s.LastSender)
	s.LastSenderGroup = stripCtl(s.LastSenderGroup)
	s.SignalTarget = stripCtl(s.SignalTarget)
	s.Channels = stripSlice(s.Channels)
	// Zero local-mux routing identifiers on remote rows: they name a satellite's
	// PTY/tmux pane, never a local one — clearing them removes any chance a mux
	// routing path writes to a local pane off a federated session.
	s.PtyID = ""
	s.TmuxTarget = ""
	// Force the origin last — never trust the wire value (C6).
	s.Origin = origin
	return s
}
