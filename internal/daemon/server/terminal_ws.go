package server

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/grovetools/tuimux/hub"
)

type TerminalHub struct {
	inner *hub.Hub
}

func NewTerminalHub(autoShutdown bool) *TerminalHub {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := hub.Config{
		AutoShutdown:   autoShutdown,
		InitialTimeout: 5 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		Logger:         logger.With("component", "groved.server.treemux"),
	}
	return &TerminalHub{inner: hub.NewHub(cfg)}
}

func (h *TerminalHub) ShutdownReq() <-chan struct{} { return h.inner.ShutdownReq() }
func (h *TerminalHub) HasConnections() bool         { return h.inner.HasConnections() }

func (s *Server) HandleTerminalWS(w http.ResponseWriter, r *http.Request) {
	if s.terminalHub != nil {
		s.terminalHub.inner.HandleWS(w, r)
	}
}
