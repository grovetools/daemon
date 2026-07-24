package server

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/grovetools/core/pkg/models"
	"github.com/grovetools/daemon/internal/daemon/store"
)

var lowerHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateSubjobEvent(ev *models.SubjobEvent) bool {
	if ev.SchemaVersion != 1 || !lowerHex64.MatchString(ev.PlanKey) || !lowerHex64.MatchString(ev.ReportSHA256) {
		return false
	}
	if ev.Kind != models.SubjobReportReady && ev.Kind != models.SubjobJoined {
		return false
	}
	if ev.ParentJobID == "" || ev.ChildJobID == "" || len(ev.ParentJobID) > 256 || len(ev.ChildJobID) > 256 {
		return false
	}
	return utf8.ValidString(ev.ParentJobID) && utf8.ValidString(ev.ChildJobID)
}

func (s *Server) handleSubjobEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var ev models.SubjobEvent
	if err := dec.Decode(&ev); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		http.Error(w, "invalid trailing data", http.StatusBadRequest)
		return
	}
	if !validateSubjobEvent(&ev) {
		http.Error(w, "invalid subjob event", http.StatusBadRequest)
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}
	kind := store.UpdateSubjobReportReady
	if ev.Kind == models.SubjobJoined {
		kind = store.UpdateSubjobJoined
	}
	s.engine.Store().ApplyUpdate(store.Update{Type: kind, Source: "flow", Payload: &ev})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(ev)
}

func (s *Server) handleGetSubjobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.engine == nil {
		http.Error(w, "engine not initialized", http.StatusServiceUnavailable)
		return
	}
	planKey, parentID := r.URL.Query().Get("plan_key"), r.URL.Query().Get("parent_job_id")
	if !lowerHex64.MatchString(planKey) || parentID == "" || len(parentID) > 256 || !utf8.ValidString(parentID) {
		http.Error(w, "plan_key and parent_job_id are required", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.engine.Store().GetSubjobSnapshot(planKey, parentID))
}
