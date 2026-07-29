package hooks

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// Event is what an `[[daemon.hooks.on_event]]` hook receives: the JSON written
// to its stdin, and the source of its GROVE_* environment variables.
//
// It is a FLATTENED projection of a store update, not the update itself. The
// daemon's payload types are internal Go structs whose shapes change with the
// subsystem that owns them; a hook author should be able to write
// `$GROVE_JOB_ID` once and have it keep working. So the handful of fields
// worth filtering and scripting on are lifted to the top level, and the raw
// payload rides along under "data" for anything else — present but explicitly
// not a stable contract.
type Event struct {
	// Type is the store update type ("job_completed", "note_event", …).
	Type string `json:"type"`
	// Seq is the bus sequence number of the originating update.
	Seq uint64 `json:"seq,omitempty"`
	// Source names the collector that produced the update.
	Source string `json:"source,omitempty"`
	// Time is when the dispatcher observed the event.
	Time time.Time `json:"time"`

	// The projected fields. Each is empty when the payload has no such notion.
	JobID     string `json:"job_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Status    string `json:"status,omitempty"`
	Origin    string `json:"origin,omitempty"`

	// Data is the raw payload as JSON. Best-effort: some payloads carry
	// unmarshalable values, in which case it is omitted rather than failing
	// the whole dispatch.
	Data json.RawMessage `json:"data,omitempty"`
}

// payloadProbe pulls the projected fields out of an already-marshaled payload.
// A JSON round trip rather than a type switch per payload type: the daemon has
// ~20 payload structs and gains more with every subsystem, and every one of
// them already spells these concepts with the same JSON keys.
type payloadProbe struct {
	ID        string `json:"id"`
	JobID     string `json:"job_id"`
	Workspace string `json:"workspace"`
	PlanName  string `json:"plan_name"`
	Plan      string `json:"plan"`
	Status    string `json:"status"`
	State     string `json:"state"`
	Origin    string `json:"origin"`
	// Event is NoteEvent's discriminator ("created", "deleted", …), which is
	// the closest thing that payload has to a status.
	Event string `json:"event"`
	// Path lets note events filter by workspace when the payload spells the
	// workspace as a directory rather than a name.
	Path string `json:"path"`
}

// NewEvent projects a store update into the hook-facing shape.
func NewEvent(u store.Update, now time.Time) Event {
	ev := Event{
		Type:   string(u.Type),
		Seq:    u.Seq,
		Source: u.Source,
		Time:   now,
		Origin: u.Origin,
	}
	if u.Payload == nil {
		return ev
	}

	data, err := json.Marshal(u.Payload)
	if err != nil {
		// A payload that will not marshal still produces a usable event: the
		// type and the sequence are often all a hook needs.
		return ev
	}
	ev.Data = data

	var probe payloadProbe
	if err := json.Unmarshal(data, &probe); err != nil {
		// Not an object (a focus update's []string, a config_reload's string).
		return ev
	}
	ev.JobID = firstNonEmpty(probe.JobID, probe.ID)
	ev.Workspace = firstNonEmpty(probe.Workspace, probe.Path)
	ev.Plan = firstNonEmpty(probe.PlanName, probe.Plan)
	ev.Status = firstNonEmpty(probe.Status, probe.State, probe.Event)
	if probe.Origin != "" {
		ev.Origin = probe.Origin
	}
	return ev
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Env renders the event as GROVE_* environment entries, mirroring the
// grove-env-<name> and Claude-hook conventions. Everything here is also in the
// stdin JSON; the variables exist so a one-line shell hook needs no jq.
func (e Event) Env() map[string]string {
	env := map[string]string{
		"GROVE_EVENT_TYPE": e.Type,
		"GROVE_EVENT_SEQ":  strconv.FormatUint(e.Seq, 10),
	}
	for key, value := range map[string]string{
		"GROVE_EVENT_SOURCE": e.Source,
		"GROVE_EVENT_ORIGIN": e.Origin,
		"GROVE_JOB_ID":       e.JobID,
		"GROVE_WORKSPACE":    e.Workspace,
		"GROVE_PLAN":         e.Plan,
		"GROVE_JOB_STATUS":   e.Status,
	} {
		if value != "" {
			env[key] = value
		}
	}
	return env
}

// field resolves a filter field name to its event value. The bool reports
// whether the name is one the filter language knows.
func (e Event) field(name string) (string, bool) {
	switch name {
	case "type", "event":
		return e.Type, true
	case "job_id", "job":
		return e.JobID, true
	case "workspace", "ws":
		return e.Workspace, true
	case "plan":
		return e.Plan, true
	case "status":
		return e.Status, true
	case "source":
		return e.Source, true
	case "origin":
		return e.Origin, true
	}
	return "", false
}

// Matcher decides whether an event triggers one configured hook. It is built
// once per config load so a malformed `events`/`filter` is reported at startup
// rather than silently never matching at runtime.
type Matcher struct {
	// Name identifies the hook in logs and as the dedupe/cancel key.
	Name string
	// events are globs over the update type. Empty never matches.
	events []string
	// terms are the parsed filter conjuncts; all must hold.
	terms []filterTerm
}

// filterTerm is one conjunct of a filter. A term with an empty Field is a
// substring match against workspace, plan and job id together.
type filterTerm struct {
	Field   string
	Pattern string
}

// NewMatcher compiles a hook's `events` and `filter` into a matcher, returning
// every problem it found. A hook with problems is still returned so the caller
// can report and skip it as a unit.
func NewMatcher(hook config.EventHook) (*Matcher, []error) {
	var problems []error

	m := &Matcher{Name: hookIdentity(hook)}
	for _, raw := range hook.Events {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		if !strings.ContainsAny(pattern, "*?") && !store.IsKnownUpdateType(store.UpdateType(pattern)) {
			// Not fatal: a hook may target an event type a NEWER daemon emits,
			// and refusing to load it would make config forward-incompatible.
			// But a typo is far more likely, so say so.
			problems = append(problems, &ConfigError{Hook: m.Name, Detail: "unknown event type " + strconv.Quote(pattern) +
				" (it will never fire on this daemon)"})
		}
		m.events = append(m.events, pattern)
	}
	if len(m.events) == 0 {
		problems = append(problems, &ConfigError{Hook: m.Name, Detail: "no events configured; the hook can never fire"})
	}

	terms, filterProblems := parseFilter(hook.Filter)
	for _, err := range filterProblems {
		problems = append(problems, &ConfigError{Hook: m.Name, Detail: err.Error()})
	}
	m.terms = terms

	return m, problems
}

// parseFilter compiles a filter string into conjuncts.
//
// The language is intentionally tiny — `field=glob` terms separated by
// whitespace or commas, ANDed, plus a bare glob as a substring match. It is
// not an expression language and is not meant to grow into one by accretion;
// see the CEL follow-up.
func parseFilter(filter string) ([]filterTerm, []error) {
	filter = normalizeFilter(filter)
	if filter == "" {
		return nil, nil
	}

	var terms []filterTerm
	var problems []error
	for _, raw := range strings.FieldsFunc(filter, func(r rune) bool { return r == ' ' || r == '\t' || r == ',' }) {
		term := strings.TrimSpace(raw)
		if term == "" {
			continue
		}
		field, pattern, hasField := strings.Cut(term, "=")
		if !hasField {
			terms = append(terms, filterTerm{Pattern: strings.Trim(term, "'\"")})
			continue
		}
		field = strings.TrimSpace(field)
		pattern = strings.Trim(strings.TrimSpace(pattern), "'\"")
		if pattern == "" {
			problems = append(problems, &ConfigError{Detail: "filter term " + strconv.Quote(term) + " has no value"})
			continue
		}
		if _, ok := (Event{}).field(field); !ok {
			problems = append(problems, &ConfigError{Detail: "unknown filter field " + strconv.Quote(field) +
				" (known: type, job_id, workspace, plan, status, source, origin)"})
			continue
		}
		terms = append(terms, filterTerm{Field: field, Pattern: pattern})
	}
	return terms, problems
}

// normalizeFilter collapses `field == 'value'` and `field = value` down to the
// canonical `field=value`, so a filter copied from the design sketch (which
// used the comparison spelling) behaves as written instead of parsing into
// three nonsense terms. It is a spelling accommodation, NOT the first step of
// an expression language: `==` is the only operator, and there is no `or`,
// no `!`, no parentheses.
func normalizeFilter(filter string) string {
	filter = strings.ReplaceAll(strings.TrimSpace(filter), "==", "=")
	var b strings.Builder
	b.Grow(len(filter))
	for i := 0; i < len(filter); i++ {
		c := filter[i]
		if c == ' ' || c == '\t' {
			// Drop whitespace that merely pads an '=' on either side.
			j := i
			for j < len(filter) && (filter[j] == ' ' || filter[j] == '\t') {
				j++
			}
			prevIsEq := b.Len() > 0 && b.String()[b.Len()-1] == '='
			nextIsEq := j < len(filter) && filter[j] == '='
			if prevIsEq || nextIsEq {
				i = j - 1
				continue
			}
			b.WriteByte(' ')
			i = j - 1
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}

// Matches reports whether the event triggers this hook.
func (m *Matcher) Matches(ev Event) bool {
	if m == nil || len(m.events) == 0 {
		return false
	}
	matched := false
	for _, pattern := range m.events {
		if globMatch(pattern, ev.Type) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, term := range m.terms {
		if !term.matches(ev) {
			return false
		}
	}
	return true
}

func (t filterTerm) matches(ev Event) bool {
	if t.Field == "" {
		// Bare term: substring over the identifying fields. Case-insensitive,
		// because workspace names in config rarely match path casing exactly.
		needle := strings.ToLower(t.Pattern)
		for _, haystack := range []string{ev.Workspace, ev.Plan, ev.JobID} {
			if haystack != "" && strings.Contains(strings.ToLower(haystack), needle) {
				return true
			}
		}
		return false
	}
	value, ok := ev.field(t.Field)
	if !ok {
		return false
	}
	if globMatch(t.Pattern, value) {
		return true
	}
	// Path-valued fields also match on their last segment, so
	// `workspace=grovetools` and `workspace=grove*` both match the workspace
	// path /Users/me/src/grovetools. Users write the name they know, not the
	// absolute path the daemon happens to carry.
	if strings.Contains(value, "/") {
		return globMatch(t.Pattern, path.Base(value))
	}
	return false
}

// globMatch is `*`/`?` wildcard matching where `*` spans ANY characters,
// separators included.
//
// path.Match is the obvious choice and the wrong one here: its `*` stops at
// `/`, so `workspace=*grovetools` would never match an absolute workspace
// path — a filter that reads as if it must work, silently matching nothing.
// Character classes are not supported; `[` is a literal.
func globMatch(pattern, value string) bool {
	// Iterative wildcard match with backtracking on the last '*'.
	var (
		p, v        int
		star        = -1
		valueAtStar int
	)
	for v < len(value) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]):
			p++
			v++
		case p < len(pattern) && pattern[p] == '*':
			star, valueAtStar = p, v
			p++
		case star >= 0:
			p = star + 1
			valueAtStar++
			v = valueAtStar
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// ConfigError is a problem found while compiling one hook's configuration.
type ConfigError struct {
	Hook   string
	Detail string
	Err    error
}

func (e *ConfigError) Error() string {
	msg := e.Detail
	if e.Hook != "" {
		msg = e.Hook + ": " + msg
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *ConfigError) Unwrap() error { return e.Err }

// hookIdentity is the stable key for a hook — used in logs, for
// cancel_previous, and for terminal-event dedupe. A hook without a name falls
// back to its command, which is at least stable across reloads.
func hookIdentity(hook config.EventHook) string {
	if name := strings.TrimSpace(hook.Name); name != "" {
		return name
	}
	return strings.TrimSpace(hook.Command)
}
