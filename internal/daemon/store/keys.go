package store

import "github.com/grovetools/core/pkg/models"

// originSep separates the origin from the bare ID in a composite Store key.
// NUL can never appear in a registry name or a job/session ID, so the composite
// key is unambiguous and can never collide with a bare local ID.
const originSep = "\x00"

// jobKey returns the map key for a job in State.Jobs (M2 contract C7). Local
// jobs (Origin == "") key by bare ID, preserving EVERY existing behavior —
// including that GetJob(bareID) resolves local rows only, which is what keeps
// the mutation/liveness paths naturally remote-safe. Federated jobs key by
// origin + NUL + ID so a same-ID local and remote row coexist without
// collision and a satellite cannot spoof or clobber a local row.
func jobKey(j *models.JobInfo) string {
	if j.Origin == "" {
		return j.ID
	}
	return j.Origin + originSep + j.ID
}

// sessionKey returns the map key for a session in State.Sessions (C7), with the
// same bare-ID-for-local / composite-for-remote rule as jobKey.
func sessionKey(s *models.Session) string {
	if s.Origin == "" {
		return s.ID
	}
	return s.Origin + originSep + s.ID
}
