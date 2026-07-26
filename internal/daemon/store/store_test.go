package store

import (
	"testing"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
)

func TestUpdateJobsDiscovered_StateTransitions(t *testing.T) {
	tests := []struct {
		name           string
		existingStatus string
		updateStatus   string
		expectedStatus string
	}{
		{
			name:           "terminal overrides running",
			existingStatus: "running",
			updateStatus:   "completed",
			expectedStatus: "completed",
		},
		{
			name:           "terminal idle overrides queued",
			existingStatus: "queued",
			updateStatus:   "idle",
			expectedStatus: "idle",
		},
		{
			name:           "terminal failed overrides running",
			existingStatus: "running",
			updateStatus:   "failed",
			expectedStatus: "failed",
		},
		{
			name:           "stale pending ignored for running",
			existingStatus: "running",
			updateStatus:   "pending",
			expectedStatus: "running",
		},
		{
			name:           "stale queued ignored for running",
			existingStatus: "running",
			updateStatus:   "queued",
			expectedStatus: "running",
		},
		{
			name:           "stale pending_user ignored for queued",
			existingStatus: "queued",
			updateStatus:   "pending_user",
			expectedStatus: "queued",
		},
		{
			name:           "stale pending ignored for queued",
			existingStatus: "queued",
			updateStatus:   "pending",
			expectedStatus: "queued",
		},
		{
			name:           "new job with no existing is accepted",
			existingStatus: "", // no existing job
			updateStatus:   "pending",
			expectedStatus: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New()

			jobID := "test-job-1"

			// Pre-populate if there's an existing status
			if tt.existingStatus != "" {
				s.state.Jobs[jobID] = &models.JobInfo{
					ID:     jobID,
					Status: tt.existingStatus,
				}
			}

			// Apply UpdateJobsDiscovered
			s.ApplyUpdate(Update{
				Type:   UpdateJobsDiscovered,
				Source: "collector",
				Payload: []*models.JobInfo{
					{ID: jobID, Status: tt.updateStatus},
				},
			})

			got := s.state.Jobs[jobID]
			if got == nil {
				t.Fatalf("expected job %s to exist in state", jobID)
			}
			if got.Status != tt.expectedStatus {
				t.Errorf("expected status %q, got %q", tt.expectedStatus, got.Status)
			}
		})
	}
}

func TestFocusLeaseExpiresAndRenewalExtendsIt(t *testing.T) {
	s := New()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }

	s.SetFocusTTL("viewer", []string{"/repo"}, time.Minute)
	if !s.IsFocused("/repo") {
		t.Fatal("new focus lease not visible")
	}
	now = now.Add(30 * time.Second)
	s.SetFocusTTL("viewer", []string{"/repo"}, time.Minute)
	now = now.Add(45 * time.Second)
	if !s.IsFocused("/repo") {
		t.Fatal("renewed focus lease expired at its original deadline")
	}
	now = now.Add(16 * time.Second)
	if s.IsFocused("/repo") || len(s.GetFocus()) != 0 {
		t.Fatal("expired focus lease remained active")
	}
}

func TestFocusEmptyPathsClearSource(t *testing.T) {
	s := New()
	s.SetFocus("viewer", []string{"/repo"})
	s.SetFocus("viewer", nil)
	if s.IsFocused("/repo") {
		t.Fatal("empty focus update did not clear source")
	}
}

func TestStore_PubSub_SingleSubscriber(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer s.Unsubscribe(sub)

	s.ApplyUpdate(Update{
		Type:   UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/test": {},
		},
	})

	select {
	case update := <-sub:
		if update.Type != UpdateWorkspaces {
			t.Errorf("expected update type %q, got %q", UpdateWorkspaces, update.Type)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for update on subscriber channel")
	}
}

func TestStore_PubSub_MultipleSubscribers(t *testing.T) {
	s := New()
	sub1 := s.Subscribe()
	sub2 := s.Subscribe()

	s.ApplyUpdate(Update{
		Type:   UpdateWorkspaces,
		Source: "test",
		Payload: map[string]*models.EnrichedWorkspace{
			"/test": {},
		},
	})

	// Both subscribers should receive the update
	for i, sub := range []chan Update{sub1, sub2} {
		select {
		case update := <-sub:
			if update.Type != UpdateWorkspaces {
				t.Errorf("subscriber %d: expected update type %q, got %q", i, UpdateWorkspaces, update.Type)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d: timed out waiting for update", i)
		}
	}

	// Unsubscribe sub1, verify sub2 still works
	s.Unsubscribe(sub1)

	s.ApplyUpdate(Update{
		Type:   UpdateSessions,
		Source: "test",
	})

	select {
	case update := <-sub2:
		if update.Type != UpdateSessions {
			t.Errorf("expected update type %q, got %q", UpdateSessions, update.Type)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("sub2 timed out after sub1 unsubscribed")
	}

	// Verify sub1 channel is closed
	_, ok := <-sub1
	if ok {
		t.Error("expected sub1 channel to be closed after Unsubscribe")
	}

	s.Unsubscribe(sub2)
}

func TestStore_JobLifecycleUpdates(t *testing.T) {
	s := New()

	job := &models.JobInfo{
		ID:     "job-abc",
		Status: "queued",
	}

	// Submit
	s.ApplyUpdate(Update{
		Type:    UpdateJobSubmitted,
		Source:  "jobrunner",
		Payload: job,
	})
	if got := s.GetJob("job-abc"); got == nil {
		t.Fatal("expected job to exist after submission")
	} else if got.Status != "queued" {
		t.Errorf("expected status queued, got %s", got.Status)
	}

	// Start
	job.Status = "running"
	s.ApplyUpdate(Update{
		Type:    UpdateJobStarted,
		Source:  "jobrunner",
		Payload: job,
	})
	if got := s.GetJob("job-abc"); got.Status != "running" {
		t.Errorf("expected status running, got %s", got.Status)
	}

	// Complete
	job.Status = "completed"
	s.ApplyUpdate(Update{
		Type:    UpdateJobCompleted,
		Source:  "jobrunner",
		Payload: job,
	})
	if got := s.GetJob("job-abc"); got.Status != "completed" {
		t.Errorf("expected status completed, got %s", got.Status)
	}
}

func TestStore_BroadcastThemeChanged(t *testing.T) {
	s := New()
	sub1 := s.Subscribe()
	sub2 := s.Subscribe()
	defer s.Unsubscribe(sub1)
	defer s.Unsubscribe(sub2)

	payload := &coredaemon.ThemeChangedPayload{
		Family: "kanagawa",
		Mode:   "hex",
		Dark:   &coredaemon.ThemePalette{Name: "kanagawa-dark", Appearance: "dark", Bg: "#1f1f28"},
	}
	s.BroadcastThemeChanged("kanagawa", payload)

	for i, sub := range []chan Update{sub1, sub2} {
		select {
		case update := <-sub:
			if update.Type != UpdateThemeChanged {
				t.Errorf("subscriber %d: expected update type %q, got %q", i, UpdateThemeChanged, update.Type)
			}
			if update.Source != "config" {
				t.Errorf("subscriber %d: expected source %q, got %q", i, "config", update.Source)
			}
			got, ok := update.Payload.(*coredaemon.ThemeChangedPayload)
			if !ok {
				t.Fatalf("subscriber %d: expected *coredaemon.ThemeChangedPayload payload, got %T", i, update.Payload)
			}
			// BroadcastThemeChanged fills an empty payload Name from the name arg.
			if got.Name != "kanagawa" {
				t.Errorf("subscriber %d: expected payload name %q, got %q", i, "kanagawa", got.Name)
			}
			if got.Dark == nil || got.Dark.Bg != "#1f1f28" {
				t.Errorf("subscriber %d: dark palette did not survive broadcast: %+v", i, got.Dark)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d: timed out waiting for theme_changed update", i)
		}
	}
}

func TestStore_BroadcastThemeChanged_NilPayloadIsDropped(t *testing.T) {
	s := New()
	sub := s.Subscribe()
	defer s.Unsubscribe(sub)

	s.BroadcastThemeChanged("kanagawa", nil)

	select {
	case update := <-sub:
		t.Fatalf("expected no update for nil payload, got %v", update.Type)
	case <-time.After(50 * time.Millisecond):
	}
}
