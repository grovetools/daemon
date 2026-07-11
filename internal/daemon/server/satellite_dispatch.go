package server

import (
	"context"
	"fmt"
	"net"
	"time"

	coredaemon "github.com/grovetools/core/pkg/daemon"
	"github.com/grovetools/core/pkg/models"
	coreplan "github.com/grovetools/core/pkg/plan"
	"github.com/grovetools/daemon/internal/daemon/satellite"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// forwardJobToSatellite ships a satellite-routed submit to the target
// satellite's existing POST /api/jobs over the SSH transport (M2 C1/C3): the
// laptop dials outward, the satellite gains no new verb. It clears the routing
// field before forwarding (so the satellite can't re-forward), sanitizes the
// remote-supplied response (C9), and writes the advisory dispatch lease (C14).
func (s *Server) forwardJobToSatellite(ctx context.Context, cm *satellite.ConnManager, req models.JobSubmitRequest) (*models.JobInfo, error) {
	name := req.Satellite

	client, err := coredaemon.NewRemoteClientWithDialer(func(context.Context) (net.Conn, error) {
		return cm.DialSatelliteSocket(name)
	})
	if err != nil {
		return nil, fmt.Errorf("building satellite client for %q: %w", name, err)
	}

	// Clear the routing field so the satellite treats this as an ordinary local
	// submit and cannot re-forward it in a loop.
	fwd := req
	fwd.Satellite = ""

	info, err := client.SubmitJob(ctx, fwd)
	if err != nil {
		return nil, fmt.Errorf("forwarding job to satellite %q: %w", name, err)
	}

	// The response is untrusted remote state (C9): strip control sequences and
	// force Origin to the registry name before it enters our Store/response.
	info = satellite.SanitizeJobInfo(info, name)

	// Advisory lease (C14): mark the LOCAL plan dir as dispatched-out so a
	// subsequent `flow plan run` into it refuses without --force. req.PlanDir is
	// the laptop-local plan directory the flow client resolved.
	s.writeSatelliteLease(ctx, req.PlanDir, name, info)

	return info, nil
}

// writeSatelliteLease writes .grove-lease.yml into the local plan dir and
// records the jobID→planDir mapping so the lease can be released when the job's
// federated terminal event arrives. Failures are logged, not fatal — the lease
// is advisory.
func (s *Server) writeSatelliteLease(ctx context.Context, planDir, name string, info *models.JobInfo) {
	if planDir == "" || info == nil || info.ID == "" {
		return
	}
	lease := coreplan.Lease{
		HolderOrigin: name,
		JobID:        info.ID,
		AcquiredAt:   time.Now(),
		TTL:          coreplan.DefaultLeaseTTL,
	}
	if err := coreplan.WriteLease(planDir, lease); err != nil {
		s.ulog.Warn("Failed to write satellite dispatch lease").
			Field("plan_dir", planDir).Field("job_id", info.ID).Err(err).Log(ctx)
		return
	}
	s.satelliteLeasesMu.Lock()
	s.satelliteLeases[info.ID] = planDir
	s.satelliteLeasesMu.Unlock()
}

// StartSatelliteLeaseReleaser subscribes to the Store and removes a dispatch
// lease when its job reaches a terminal federated state (M2 C14). It runs on
// the global daemon only (wired under the scope=="" gate in groved.go). The
// goroutine exits when ctx is cancelled.
func (s *Server) StartSatelliteLeaseReleaser(ctx context.Context) {
	if s.engine == nil {
		return
	}
	st := s.engine.Store()
	if st == nil {
		return
	}
	ch := st.Subscribe()
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
					// Only remote-origin (satellite) jobs carry a lease.
					continue
				}
				s.releaseSatelliteLease(ctx, job.ID)
			}
		}
	}()
}

// releaseSatelliteLease removes the lease recorded for jobID, if any.
func (s *Server) releaseSatelliteLease(ctx context.Context, jobID string) {
	s.satelliteLeasesMu.Lock()
	planDir, ok := s.satelliteLeases[jobID]
	if ok {
		delete(s.satelliteLeases, jobID)
	}
	s.satelliteLeasesMu.Unlock()
	if !ok {
		return
	}
	if err := coreplan.RemoveLease(planDir); err != nil {
		s.ulog.Warn("Failed to remove satellite dispatch lease").
			Field("plan_dir", planDir).Field("job_id", jobID).Err(err).Log(ctx)
	}
}

// isTerminalJobUpdate reports whether an update type marks a job as finished.
func isTerminalJobUpdate(t store.UpdateType) bool {
	switch t {
	case store.UpdateJobCompleted, store.UpdateJobFailed, store.UpdateJobCancelled:
		return true
	default:
		return false
	}
}
