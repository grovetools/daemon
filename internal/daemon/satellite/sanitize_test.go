package satellite

import (
	"testing"

	"github.com/grovetools/core/pkg/models"
)

func TestStripCtl(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"csi color", "\x1b[31mred\x1b[0m", "red"},
		{"osc title bel", "\x1b]0;evil title\x07keep", "keep"},
		{"osc title st", "\x1b]0;evil\x1b\\keep", "keep"},
		{"bare c0", "a\x00b\x07c\x1fd", "abcd"},
		{"newlines and tabs dropped", "a\nb\tc", "abc"},
		{"del byte", "a\x7fb", "ab"},
		{"cursor move", "\x1b[2Jclear", "clear"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := stripCtl(tc.in); got != tc.want {
			t.Errorf("%s: stripCtl(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestSanitizeJobInfoForcesOrigin proves ANSI is stripped from every string
// field and Origin is forced to the registry name regardless of the wire value.
func TestSanitizeJobInfoForcesOrigin(t *testing.T) {
	j := &models.JobInfo{
		ID:     "\x1b[31mjob1\x1b[0m",
		Title:  "evil\x1b]0;spoof\x07 title",
		Status: "run\x00ning",
		Origin: "attacker-claimed", // wire value must be discarded
		Env:    map[string]string{"K\x1b[0mEY": "va\x07l"},
	}
	SanitizeJobInfo(j, "sat")

	if j.ID != "job1" {
		t.Errorf("ID not sanitized: %q", j.ID)
	}
	if j.Title != "evil title" {
		t.Errorf("Title not sanitized: %q", j.Title)
	}
	if j.Status != "running" {
		t.Errorf("Status not sanitized: %q", j.Status)
	}
	if j.Origin != "sat" {
		t.Errorf("Origin not forced to registry name: %q (spoof leaked)", j.Origin)
	}
	if _, ok := j.Env["KEY"]; !ok {
		t.Errorf("Env key not sanitized: %+v", j.Env)
	}
}

// TestSanitizeSessionZeroesMuxRouting proves remote sessions get their local-mux
// routing identifiers zeroed and Origin forced.
func TestSanitizeSessionZeroesMuxRouting(t *testing.T) {
	s := &models.Session{
		ID:          "s1",
		JobTitle:    "\x1b[1mbold\x1b[0m",
		ParentJobID: "parent\njob",
		PtyID:       "local-pty-42",
		TmuxTarget:  "grove:1.2",
		Origin:      "spoofed",
	}
	SanitizeSession(s, "sat")

	if s.JobTitle != "bold" {
		t.Errorf("JobTitle not sanitized: %q", s.JobTitle)
	}
	if s.ParentJobID != "parentjob" {
		t.Errorf("ParentJobID not sanitized: %q", s.ParentJobID)
	}
	if s.PtyID != "" {
		t.Errorf("PtyID must be zeroed on remote session, got %q", s.PtyID)
	}
	if s.TmuxTarget != "" {
		t.Errorf("TmuxTarget must be zeroed on remote session, got %q", s.TmuxTarget)
	}
	if s.Origin != "sat" {
		t.Errorf("Origin not forced: %q", s.Origin)
	}
}

func TestSanitizeNilSafe(t *testing.T) {
	if SanitizeJobInfo(nil, "sat") != nil {
		t.Error("SanitizeJobInfo(nil) should return nil")
	}
	if SanitizeSession(nil, "sat") != nil {
		t.Error("SanitizeSession(nil) should return nil")
	}
}
