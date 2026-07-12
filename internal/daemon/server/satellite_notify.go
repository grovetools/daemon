package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/grovetools/core/pkg/models"
	notifications "github.com/grovetools/notify"

	"github.com/grovetools/daemon/internal/daemon/store"
)

// StartSatelliteNotifier subscribes to the Store and fires a cross-machine
// notification when a REMOTE (satellite-origin) job reaches a terminal state
// (M2 contract C18, the ntfy-primary bridge). It copies P9's
// StartSatelliteLeaseReleaser subscriber shape and, like it, runs on the global
// daemon only (wired under the scope=="" gate in groved.go). The goroutine
// exits when ctx is cancelled.
//
// ntfy is the PRIMARY transport because it reaches the operator's phone/desktop
// regardless of where the laptop daemon runs; notify.SendSystem is a best-effort
// adjunct whose osascript/notify-send delegate no-ops on a headless host. Empty
// ntfyURL/ntfyTopic disables the primary send (system notify still fires).
func (s *Server) StartSatelliteNotifier(ctx context.Context, ntfyURL, ntfyTopic string) {
	if s.engine == nil {
		return
	}
	st := s.engine.Store()
	if st == nil {
		return
	}
	ch := st.Subscribe()
	// notified dedupes by job ID: the Store synthesizes a per-job terminal
	// event from every snapshot diff that shows the transition (B1), so a job
	// that drops out of one snapshot and reappears terminal in a later one
	// would re-fire. Lease release is idempotent; notifications are not.
	var mu sync.Mutex
	notified := make(map[string]struct{})
	// Sink seam: tests observe the bridge here instead of firing real
	// osascript/ntfy I/O. Production leaves the field nil.
	notify := s.satelliteNotifyFn
	if notify == nil {
		notify = s.notifySatelliteTerminal
	}
	go func() {
		defer st.Unsubscribe(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case upd, ok := <-ch:
				if !ok {
					return
				}
				if !isTerminalJobUpdate(upd.Type) {
					continue
				}
				job, ok := upd.Payload.(*models.JobInfo)
				if !ok || job == nil || job.Origin == "" {
					// Only remote-origin (satellite) jobs are federated cross-machine.
					continue
				}
				mu.Lock()
				if _, seen := notified[job.ID]; seen {
					mu.Unlock()
					continue
				}
				notified[job.ID] = struct{}{}
				mu.Unlock()

				notify(ctx, job, upd.Type, ntfyURL, ntfyTopic)
			}
		}
	}()
}

// notifySatelliteTerminal renders and dispatches a single terminal-job
// notification: ntfy as the reliable primary (skipped silently when unconfigured),
// a best-effort system notification as adjunct.
func (s *Server) notifySatelliteTerminal(ctx context.Context, job *models.JobInfo, updType store.UpdateType, ntfyURL, ntfyTopic string) {
	status := satelliteTerminalStatus(updType)
	label := job.JobFile
	if label == "" {
		label = job.ID
	}
	title := fmt.Sprintf("satellite %s: %s/%s %s", job.Origin, job.PlanName, label, status)
	msg := fmt.Sprintf("Job %s on satellite %q %s", label, job.Origin, status)

	priority := "default"
	tags := []string{"satellite"}
	if updType == store.UpdateJobFailed {
		priority = "high"
		tags = append(tags, "warning")
	}

	// Primary: ntfy. Skip silently when URL/topic are unconfigured — the
	// notifier itself also errors on empty URL/topic, but avoiding the call
	// keeps the no-config path quiet.
	if ntfyURL != "" && ntfyTopic != "" {
		if err := notifications.SendNtfy(ntfyURL, ntfyTopic, title, msg, priority, tags); err != nil {
			if s.ulog != nil {
				s.ulog.Debug("satellite ntfy notification failed").
					Field("job", job.ID).Field("origin", job.Origin).Err(err).Log(ctx)
			}
		}
	}

	// Adjunct: best-effort system notification. Its osascript/notify-send
	// delegate no-ops on a headless host; never treat its error as a failure.
	_ = notifications.SendSystem(title, msg, satelliteNotifyLevel(updType))
}

// satelliteTerminalStatus maps a terminal update type to a short display word.
func satelliteTerminalStatus(t store.UpdateType) string {
	switch t {
	case store.UpdateJobCompleted:
		return "completed"
	case store.UpdateJobFailed:
		return "failed"
	case store.UpdateJobCancelled:
		return "cancelled"
	default:
		return "finished"
	}
}

// satelliteNotifyLevel maps a terminal update type to a system-notification level.
func satelliteNotifyLevel(t store.UpdateType) string {
	if t == store.UpdateJobFailed {
		return "error"
	}
	return "info"
}
