package server

import "net/http"

func (s *Server) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	if s.terminalHub != nil {
		s.terminalHub.inner.HandleSSE(w, r)
	}
}
