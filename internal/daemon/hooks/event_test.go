package hooks

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/grovetools/core/config"
	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

func eventHook(events []string, filter string) config.EventHook {
	return config.EventHook{
		HookCommand: config.HookCommand{Name: "test", Command: "true"},
		Events:      events,
		Filter:      filter,
	}
}

func mustMatcher(t *testing.T, hook config.EventHook) *Matcher {
	t.Helper()
	m, problems := NewMatcher(hook)
	for _, p := range problems {
		t.Fatalf("unexpected config problem: %v", p)
	}
	return m
}

func TestMatcherEventGlobs(t *testing.T) {
	cases := []struct {
		events    []string
		eventType string
		want      bool
	}{
		{[]string{"job_completed"}, "job_completed", true},
		{[]string{"job_completed"}, "job_failed", false},
		{[]string{"job_*"}, "job_failed", true},
		{[]string{"job_*"}, "note_event", false},
		{[]string{"job_*", "note_event"}, "note_event", true},
		{[]string{"*"}, "anything_at_all", true},
	}
	for _, tc := range cases {
		m, _ := NewMatcher(eventHook(tc.events, ""))
		if got := m.Matches(Event{Type: tc.eventType}); got != tc.want {
			t.Errorf("events %v vs %q = %v, want %v", tc.events, tc.eventType, got, tc.want)
		}
	}
}

// A hook with no events must never fire. Defaulting to "everything" would turn
// a config typo into arbitrary shell on every daemon heartbeat.
func TestMatcherWithNoEventsNeverFires(t *testing.T) {
	m, problems := NewMatcher(eventHook(nil, ""))
	if len(problems) == 0 {
		t.Error("an empty events list must be reported as a configuration problem")
	}
	if m.Matches(Event{Type: "job_completed"}) {
		t.Fatal("a hook with no events matched an event")
	}
}

func TestMatcherRejectsUnknownEventTypes(t *testing.T) {
	_, problems := NewMatcher(eventHook([]string{"job_compelted"}, ""))
	if len(problems) == 0 {
		t.Fatal("a misspelled event type must be reported")
	}
	// Globs are not checked against the vocabulary — they are how a config
	// stays forward-compatible with event types a newer daemon emits.
	if _, problems := NewMatcher(eventHook([]string{"future_*"}, "")); len(problems) != 0 {
		t.Errorf("a glob must not be rejected for not naming a known type: %v", problems)
	}
}

func TestFilterTerms(t *testing.T) {
	ev := Event{
		Type:      "job_completed",
		JobID:     "job-42",
		Workspace: "/Users/me/src/grovetools",
		Plan:      "grove-extensiblity",
		Status:    "completed",
		Source:    "jobrunner",
	}
	cases := []struct {
		filter string
		want   bool
	}{
		{"", true},
		{"workspace=*grovetools", true}, // '*' spans separators, unlike path.Match
		{"workspace=grove*", true},      // ...and path fields also match on their last segment
		{"workspace=grovetools", true},
		{"workspace=other*", false},
		{"plan=grove-extensib*", true},
		{"plan=other", false},
		{"status=completed", true},
		{"status=failed", false},
		{"plan=grove-* status=completed", true},
		{"plan=grove-* status=failed", false},
		{"plan=grove-*,status=completed", true}, // commas separate too
		{"job_id=job-42", true},
		{"source=jobrunner", true},
		{"origin=sat-1", false},
		{"grovetools", true},                // bare term: substring
		{"GROVETOOLS", true},                // ...case-insensitively
		{"nonesuch", false},                 // ...that does not match
		{"job-42", true},                    // ...over job id too
		{"workspace == 'grovetools'", true}, // the design sketch's spelling
	}
	for _, tc := range cases {
		m, problems := NewMatcher(eventHook([]string{"job_*"}, tc.filter))
		for _, p := range problems {
			t.Fatalf("filter %q: %v", tc.filter, p)
		}
		if got := m.Matches(ev); got != tc.want {
			t.Errorf("filter %q = %v, want %v", tc.filter, got, tc.want)
		}
	}
}

func TestFilterRejectsUnknownFields(t *testing.T) {
	_, problems := NewMatcher(eventHook([]string{"job_*"}, "wrokspace=grove"))
	if len(problems) == 0 {
		t.Fatal("an unknown filter field must be reported, not silently never match")
	}
}

// The filter is a conjunction: a hook narrowed on two fields must require both.
func TestFilterTermsAreAnded(t *testing.T) {
	m := mustMatcher(t, eventHook([]string{"job_*"}, "plan=alpha status=failed"))
	if m.Matches(Event{Type: "job_failed", Plan: "alpha", Status: "completed"}) {
		t.Error("a hook matched with only one of two filter terms satisfied")
	}
	if !m.Matches(Event{Type: "job_failed", Plan: "alpha", Status: "failed"}) {
		t.Error("a hook did not match with both terms satisfied")
	}
}

func TestNewEventProjectsJobFields(t *testing.T) {
	job := &models.JobInfo{
		ID:       "job-7",
		PlanName: "grove-extensiblity",
		Status:   "completed",
		Origin:   "sat-1",
	}
	ev := NewEvent(store.Update{
		Type:    store.UpdateJobCompleted,
		Seq:     12,
		Source:  "jobrunner",
		Payload: job,
	}, time.Now())

	if ev.Type != "job_completed" || ev.Seq != 12 || ev.Source != "jobrunner" {
		t.Fatalf("event header wrong: %+v", ev)
	}
	if ev.JobID != "job-7" || ev.Plan != "grove-extensiblity" || ev.Status != "completed" || ev.Origin != "sat-1" {
		t.Fatalf("job fields not projected: %+v", ev)
	}
	// The raw payload rides along for anything the projection does not cover.
	var raw map[string]any
	if err := json.Unmarshal(ev.Data, &raw); err != nil {
		t.Fatalf("data is not decodable JSON: %v", err)
	}
	if raw["id"] != "job-7" {
		t.Errorf("raw payload lost the job: %v", raw)
	}
}

func TestNewEventProjectsNoteFields(t *testing.T) {
	ev := NewEvent(store.Update{
		Type:   store.UpdateNoteEvent,
		Source: "notes",
		Payload: &models.NoteEvent{
			Event:     models.NoteEventCreated,
			Workspace: "grovetools",
			Path:      "/notes/a.md",
		},
	}, time.Now())

	if ev.Workspace != "grovetools" {
		t.Errorf("workspace = %q", ev.Workspace)
	}
	if ev.Status != string(models.NoteEventCreated) {
		t.Errorf("status = %q, want the note event kind", ev.Status)
	}
}

// A payload that is not an object (config_reload's string, focus's []string)
// must still produce a usable event rather than failing the dispatch.
func TestNewEventToleratesScalarPayloads(t *testing.T) {
	ev := NewEvent(store.Update{Type: store.UpdateConfigReload, Payload: "/etc/grove.toml"}, time.Now())
	if ev.Type != "config_reload" {
		t.Fatalf("type = %q", ev.Type)
	}
	if ev.JobID != "" || ev.Workspace != "" {
		t.Errorf("scalar payload produced phantom fields: %+v", ev)
	}
	if string(ev.Data) != `"/etc/grove.toml"` {
		t.Errorf("data = %s", ev.Data)
	}

	ev = NewEvent(store.Update{Type: store.UpdateFocus, Payload: []string{"/a", "/b"}}, time.Now())
	if ev.Type != "focus" {
		t.Fatalf("type = %q", ev.Type)
	}
}

func TestEventEnv(t *testing.T) {
	ev := Event{
		Type: "job_failed", Seq: 9, Source: "jobrunner",
		JobID: "job-1", Workspace: "/ws", Plan: "p", Status: "failed",
	}
	env := ev.Env()
	want := map[string]string{
		"GROVE_EVENT_TYPE":   "job_failed",
		"GROVE_EVENT_SEQ":    "9",
		"GROVE_EVENT_SOURCE": "jobrunner",
		"GROVE_JOB_ID":       "job-1",
		"GROVE_WORKSPACE":    "/ws",
		"GROVE_PLAN":         "p",
		"GROVE_JOB_STATUS":   "failed",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	// Empty fields are omitted rather than exported as "", so a hook can use
	// ${GROVE_PLAN:-none} the way shell users expect.
	if _, present := (Event{Type: "x"}).Env()["GROVE_PLAN"]; present {
		t.Error("an empty plan was exported")
	}
}

func TestHookIdentityFallsBackToCommand(t *testing.T) {
	if got := hookIdentity(config.EventHook{HookCommand: config.HookCommand{Command: "echo hi"}}); got != "echo hi" {
		t.Errorf("identity = %q", got)
	}
	if got := hookIdentity(config.EventHook{HookCommand: config.HookCommand{Name: "n", Command: "echo hi"}}); got != "n" {
		t.Errorf("identity = %q", got)
	}
}
