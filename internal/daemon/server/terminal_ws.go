package server

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// WsMessage is the JSON envelope for all WebSocket communication
// between Primary/Follower groveterm instances and the daemon hub.
type WsMessage struct {
	Type    string          `json:"type"`    // "register", "role", "layout", "frame", "input", "action", "primary_disconnected"
	Payload json.RawMessage `json:"payload"` // type-specific JSON payload
}

// TerminalHub routes WebSocket messages between a single Primary
// groveterm instance and zero or more Follower instances.
// Thread-safe: all access to connections is protected by mu.
type TerminalHub struct {
	mu        sync.RWMutex
	primary   *websocket.Conn
	followers map[*websocket.Conn]bool
	logger    *logrus.Entry
}

// NewTerminalHub creates a ready-to-use TerminalHub.
func NewTerminalHub(logger *logrus.Entry) *TerminalHub {
	return &TerminalHub{
		followers: make(map[*websocket.Conn]bool),
		logger:    logger.WithField("component", "terminal-hub"),
	}
}

// upgrader allows any origin for local unix-socket connections.
var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// HandleTerminalWS upgrades an HTTP request to a WebSocket connection
// and runs the read loop for routing messages between Primary and Followers.
func (s *Server) HandleTerminalWS(w http.ResponseWriter, r *http.Request) {
	hub := s.terminalHub
	if hub == nil {
		http.Error(w, "terminal hub not initialized", http.StatusServiceUnavailable)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		hub.logger.WithError(err).Error("WebSocket upgrade failed")
		return
	}

	hub.logger.Debug("New WebSocket connection")

	// Track whether this connection is primary so cleanup knows what to do.
	var isPrimary bool

	defer func() {
		hub.removeConn(conn, isPrimary)
		conn.Close()
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				hub.logger.WithError(err).Debug("WebSocket read error")
			}
			return
		}

		var msg WsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			hub.logger.WithError(err).Debug("Invalid WsMessage envelope")
			continue
		}

		switch msg.Type {
		case "register":
			isPrimary = hub.register(conn)

		case "layout", "frame":
			// Only the Primary may broadcast layout/frame data.
			hub.mu.RLock()
			sender := conn == hub.primary
			hub.mu.RUnlock()
			if !sender {
				hub.logger.Debug("Non-primary attempted to send layout/frame")
				continue
			}
			hub.broadcastToFollowers(raw)

		case "input", "action":
			// Only Followers may send input/action to the Primary.
			hub.mu.RLock()
			_, isFollower := hub.followers[conn]
			hub.mu.RUnlock()
			if !isFollower {
				hub.logger.Debug("Non-follower attempted to send input/action")
				continue
			}
			hub.sendToPrimary(raw)

		default:
			hub.logger.WithField("type", msg.Type).Debug("Unknown WsMessage type")
		}
	}
}

// register assigns conn as Primary (if vacant) or Follower.
// Returns true if the connection became Primary.
func (h *TerminalHub) register(conn *websocket.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	var role string
	if h.primary == nil {
		h.primary = conn
		role = "primary"
		h.logger.Info("Primary registered")
	} else {
		h.followers[conn] = true
		role = "follower"
		h.logger.Info("Follower registered")
	}

	resp, _ := json.Marshal(WsMessage{
		Type:    "role",
		Payload: json.RawMessage(`"` + role + `"`),
	})
	conn.WriteMessage(websocket.TextMessage, resp)
	return role == "primary"
}

// broadcastToFollowers sends raw to every connected Follower.
// Disconnected followers are cleaned up on their next read error.
func (h *TerminalHub) broadcastToFollowers(raw []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.followers {
		if err := c.WriteMessage(websocket.TextMessage, raw); err != nil {
			h.logger.WithError(err).Debug("Failed to write to follower")
			// Don't remove here — the follower's read loop will detect the
			// broken connection and clean up via removeConn.
		}
	}
}

// sendToPrimary forwards raw to the Primary connection.
func (h *TerminalHub) sendToPrimary(raw []byte) {
	h.mu.RLock()
	primary := h.primary
	h.mu.RUnlock()

	if primary == nil {
		return
	}
	if err := primary.WriteMessage(websocket.TextMessage, raw); err != nil {
		h.logger.WithError(err).Debug("Failed to write to primary")
	}
}

// removeConn cleans up a disconnected connection.
// If the Primary disconnects, all Followers are notified.
func (h *TerminalHub) removeConn(conn *websocket.Conn, isPrimary bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if isPrimary && h.primary == conn {
		h.primary = nil
		h.logger.Info("Primary disconnected")

		// Notify all followers that the primary is gone.
		msg, _ := json.Marshal(WsMessage{
			Type:    "primary_disconnected",
			Payload: json.RawMessage("{}"),
		})
		for c := range h.followers {
			_ = c.WriteMessage(websocket.TextMessage, msg)
		}
	} else {
		delete(h.followers, conn)
		h.logger.Debug("Follower disconnected")
	}
}
