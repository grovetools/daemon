package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/buildqueue"
)

// SetBuildScheduler wires the machine-wide build queue scheduler into the
// server so /api/build/* endpoints can reach it.
func (s *Server) SetBuildScheduler(sched *buildqueue.Scheduler) {
	s.buildScheduler = sched
}

// handleBuildSubmit handles POST /api/build/submit — enqueue one build job
// on the machine-wide build queue and return its job ID.
func (s *Server) handleBuildSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.buildScheduler == nil {
		http.Error(w, "build queue not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.BuildJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	jobID, err := s.buildScheduler.Submit(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(models.BuildSubmitResponse{JobID: jobID})
}

// handleBuildCancel handles POST /api/build/cancel — kill the running
// process groups of a submission group and drain its queued jobs.
func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.buildScheduler == nil {
		http.Error(w, "build queue not initialized", http.StatusServiceUnavailable)
		return
	}

	var req models.BuildCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.GroupID == "" {
		http.Error(w, "group_id is required", http.StatusBadRequest)
		return
	}

	n := s.buildScheduler.Cancel(req.GroupID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "cancelled", "jobs": n})
}

// handleBuildJobSubpath routes /api/build/jobs/{id}/stream.
func (s *Server) handleBuildJobSubpath(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/build/jobs/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "stream" {
		s.handleStreamBuildJob(w, r, parts[0])
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// handleStreamBuildJob provides SSE streaming of a single build job's
// lifecycle + output events (pattern of handleStreamJobLogs). The buffered
// history is replayed first, so late subscribers see the full stream; the
// connection closes after the terminal "finished" event.
func (s *Server) handleStreamBuildJob(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.buildScheduler == nil {
		http.Error(w, "build queue not initialized", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	history, ch, err := s.buildScheduler.Subscribe(jobID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer s.buildScheduler.Unsubscribe(jobID, ch)

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send connection confirmation
	_, _ = fmt.Fprintf(w, ": connected to build job %s stream\n\n", jobID)
	flusher.Flush()

	writeEvent := func(ev models.BuildJobEvent) bool {
		data, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		_, _ = fmt.Fprintf(w, "event: build\ndata: %s\n\n", data)
		return ev.Event == models.BuildEventFinished
	}

	// Replay buffered history
	finished := false
	for _, ev := range history {
		if writeEvent(ev) {
			finished = true
		}
	}
	flusher.Flush()
	if finished {
		return
	}

	// Stream live events until the job reaches a terminal state
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // Stream closed (job finished)
			}
			done := writeEvent(ev)
			flusher.Flush()
			if done {
				return
			}
		}
	}
}
