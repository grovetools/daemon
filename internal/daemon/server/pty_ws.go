package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/grovetools/daemon/internal/daemon/pty"
)

// handlePtyCreate handles POST /api/pty/create.
func (s *Server) handlePtyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ptyManager == nil {
		http.Error(w, "pty manager not initialized", http.StatusServiceUnavailable)
		return
	}

	var req pty.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.CWD == "" {
		http.Error(w, "cwd is required", http.StatusBadRequest)
		return
	}

	sess, err := s.ptyManager.Create(req)
	if err != nil {
		s.ulog.Error("Failed to create PTY session").Err(err).Log(r.Context())
		http.Error(w, "failed to create session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sess.Metadata())
}

// handlePtyList handles GET /api/pty/list.
func (s *Server) handlePtyList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ptyManager == nil {
		http.Error(w, "pty manager not initialized", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.ptyManager.List())
}

// handlePtyKill handles POST /api/pty/kill/{id}.
func (s *Server) handlePtyKill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.ptyManager == nil {
		http.Error(w, "pty manager not initialized", http.StatusServiceUnavailable)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/pty/kill/")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}

	if err := s.ptyManager.Kill(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handlePtyAttach handles GET /api/pty/attach/{id} — upgrades to WebSocket.
// Binary frames carry raw PTY I/O. Text frames carry JSON control messages.
func (s *Server) handlePtyAttach(w http.ResponseWriter, r *http.Request) {
	if s.ptyManager == nil {
		http.Error(w, "pty manager not initialized", http.StatusServiceUnavailable)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/pty/attach/")
	if id == "" {
		http.Error(w, "session id is required", http.StatusBadRequest)
		return
	}

	sess, ok := s.ptyManager.Get(id)
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	ctx := r.Context()
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.ulog.Error("PTY WebSocket upgrade failed").Err(err).Log(ctx)
		return
	}

	// Replay history buffer before registering as a live client so the
	// new connection gets recent terminal output for state reconstruction.
	if hist := sess.History(); len(hist) > 0 {
		_ = conn.WriteMessage(websocket.BinaryMessage, hist)
	}

	sess.AddClient(conn)
	defer func() {
		sess.RemoveClient(conn)
		conn.Close()
	}()

	// Read loop: client → PTY
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				s.ulog.Debug("PTY WebSocket read error").Err(err).Log(ctx)
			}
			return
		}

		switch msgType {
		case websocket.BinaryMessage:
			// Raw keyboard input → PTY
			if _, err := sess.Write(data); err != nil {
				s.ulog.Debug("Failed to write to PTY").Err(err).Log(ctx)
				return
			}

		case websocket.TextMessage:
			// JSON control message
			var ctrl pty.ControlMessage
			if err := json.Unmarshal(data, &ctrl); err != nil {
				s.ulog.Debug("Invalid PTY control message").Err(err).Log(ctx)
				continue
			}
			switch ctrl.Type {
			case "resize":
				if ctrl.Rows > 0 && ctrl.Cols > 0 {
					_ = sess.Resize(ctrl.Rows, ctrl.Cols)
				}
			}
		}
	}
}
