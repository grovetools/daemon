package server

import (
	"fmt"
	"net/http"
)

// handleTerminalStream provides Server-Sent Events for web-based terminal
// viewers. It subscribes to the TerminalHub and forwards base64-encoded
// binary frame payloads (and layout events) to the browser.
func (s *Server) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	hub := s.terminalHub
	if hub == nil {
		http.Error(w, "terminal hub not initialized", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := hub.SubscribeSSE()
	defer hub.UnsubscribeSSE(ch)

	// Confirm connection
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	hub.logger.Debug("Terminal SSE client connected")

	// Ask the Primary terminal to produce a full-screen payload so this
	// new client gets a complete initial frame instead of waiting for the
	// next partial delta.
	hub.RequestFullSync()

	for {
		select {
		case <-r.Context().Done():
			hub.logger.Debug("Terminal SSE client disconnected")
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprint(w, data)
			flusher.Flush()
		}
	}
}
