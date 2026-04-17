package collector

import (
	"context"
	"time"

	"github.com/grovetools/core/logging"
	"github.com/grovetools/core/pkg/process"
	"github.com/grovetools/core/pkg/sessions"
	"github.com/grovetools/daemon/internal/daemon/store"
)

// SessionCollector monitors active sessions in the store for process liveness.
// It also performs initial crash recovery on daemon startup.
//
// The daemon store is the single source of truth for session state.
// This collector only:
// 1. Recovers sessions from the filesystem crash-recovery registry on startup
// 2. Periodically verifies that active sessions' PIDs are still alive
// 3. Cleans up dead sessions (marks as interrupted, removes crash-recovery files)
type SessionCollector struct {
	interval time.Duration
	ulog     *logging.UnifiedLogger
}

// NewSessionCollector creates a new SessionCollector.
// Defaults to 2 seconds for PID verification.
func NewSessionCollector(interval time.Duration) *SessionCollector {
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &SessionCollector{
		interval: interval,
		ulog:     logging.NewUnifiedLogger("groved.collector.session"),
	}
}

// Name returns the collector's name.
func (c *SessionCollector) Name() string { return "session" }

// Run starts the session liveness verification loop.
func (c *SessionCollector) Run(ctx context.Context, st *store.Store, updates chan<- store.Update) error {
	// 1. Initial Crash Recovery
	// Load sessions that were running before the daemon started/restarted.
	recoveredSessions, err := sessions.RecoverSessions()
	if err != nil {
		c.ulog.Warn("Failed to recover sessions from disk").Err(err).Log(ctx)
	} else if len(recoveredSessions) > 0 {
		c.ulog.Info("Recovered active sessions from crash registry").
			Field("count", len(recoveredSessions)).
			Log(ctx)
		updates <- store.Update{
			Type:    store.UpdateSessions,
			Source:  "session_recovery",
			Scanned: len(recoveredSessions),
			Payload: recoveredSessions,
		}
	}

	// 2. PID Verification Loop
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	registry, _ := sessions.NewFileSystemRegistry()

	c.ulog.Info("Session liveness collector started").Log(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			start := time.Now()

			// Get all active sessions from the canonical store
			activeSessions := st.GetSessions()

			for _, session := range activeSessions {
				// Only verify sessions we think are active
				if session.Status == "running" || session.Status == "idle" || session.Status == "pending_user" {

					// Grace period: skip PID check for sessions confirmed within the last 30s.
					// During agent startup, the initial PID may be a short-lived intermediate
					// process (shell, grove meta-tool) that exits before the real agent starts.
					if time.Since(session.LastActivity) < 30*time.Second && time.Since(session.StartedAt) < 30*time.Second {
						continue
					}

					// PID 0 means the real PID was never discovered (e.g., groveterm-native agent crashed
					// before async PID discovery completed). After the grace period, treat as dead.
					if session.PID == 0 || !process.IsProcessAlive(session.PID) {
						c.ulog.Warn("Session process died unexpectedly").
							Field("job_id", session.ID).
							Field("pid", session.PID).
							Log(ctx)

						// Update daemon state
						updates <- store.Update{
							Type:   store.UpdateSessionEnd,
							Source: "session_collector",
							Payload: &store.SessionEndPayload{
								JobID:   session.ID,
								Outcome: "interrupted",
							},
						}

						// Clean up the crash recovery files
						if registry != nil {
							nativeID := session.ClaudeSessionID
							if nativeID == "" {
								nativeID = session.ID
							}
							_ = registry.Unregister(nativeID)
						}
					}
				}
			}

			if d := time.Since(start); d > 1*time.Second {
				c.ulog.Debug("Slow PID verification detected").Field("duration", d).Log(ctx)
			}
		}
	}
}
