package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/grovetools/core/logging"
)

// WsMessage is the JSON envelope for all WebSocket communication
// between Primary/Follower groveterm instances and the daemon hub.
type WsMessage struct {
	Type    string          `json:"type"`    // "register", "role", "layout", "frame", "input", "action", "request_full_sync", "primary_disconnected"
	Payload json.RawMessage `json:"payload"` // type-specific JSON payload
}

// TerminalHub routes WebSocket messages between a single Primary
// groveterm instance and zero or more Follower instances.
// It also fans out frame data to SSE subscribers (web viewers).
// Thread-safe: all access to connections is protected by mu.
type TerminalHub struct {
	mu             sync.RWMutex
	primary        *websocket.Conn
	followers      map[*websocket.Conn]bool
	sseSubscribers map[chan string]bool
	ulog           *logging.UnifiedLogger
}

// NewTerminalHub creates a ready-to-use TerminalHub.
func NewTerminalHub() *TerminalHub {
	return &TerminalHub{
		followers:      make(map[*websocket.Conn]bool),
		sseSubscribers: make(map[chan string]bool),
		ulog:           logging.NewUnifiedLogger("groved.server.treemux"),
	}
}

// HasConnections reports whether any groveterm instance (primary or follower)
// is currently connected via WebSocket.
func (h *TerminalHub) HasConnections() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.primary != nil || len(h.followers) > 0
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

	ctx := r.Context()
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		hub.ulog.Error("WebSocket upgrade failed").Err(err).Log(ctx)
		return
	}

	hub.ulog.Debug("New WebSocket connection").Log(ctx)

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
				hub.ulog.Debug("WebSocket read error").Err(err).Log(ctx)
			}
			return
		}

		var msg WsMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			hub.ulog.Debug("Invalid WsMessage envelope").Err(err).Log(ctx)
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
				hub.ulog.Debug("Non-primary attempted to send layout/frame").Log(ctx)
				continue
			}
			hub.broadcastToFollowers(raw)

		case "input", "action":
			// Only Followers may send input/action to the Primary.
			hub.mu.RLock()
			_, isFollower := hub.followers[conn]
			hub.mu.RUnlock()
			if !isFollower {
				hub.ulog.Debug("Non-follower attempted to send input/action").Log(ctx)
				continue
			}
			hub.sendToPrimary(raw)

		default:
			hub.ulog.Debug("Unknown WsMessage type").Field("type", msg.Type).Log(ctx)
		}
	}
}

// register assigns conn as Primary (if vacant) or Follower.
// Returns true if the connection became Primary.
func (h *TerminalHub) register(conn *websocket.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	bgCtx := context.Background()
	var role string
	if h.primary == nil {
		h.primary = conn
		role = "primary"
		h.ulog.Info("Primary registered").Log(bgCtx)
	} else {
		h.followers[conn] = true
		role = "follower"
		h.ulog.Info("Follower registered").Log(bgCtx)

		// Notify the Primary that a new Follower joined so it can
		// broadcast its current layout and screen state.
		if h.primary != nil {
			notify, _ := json.Marshal(WsMessage{
				Type:    "follower_joined",
				Payload: json.RawMessage("{}"),
			})
			h.primary.WriteMessage(websocket.TextMessage, notify)
		}
	}

	resp, _ := json.Marshal(WsMessage{
		Type:    "role",
		Payload: json.RawMessage(`"` + role + `"`),
	})
	conn.WriteMessage(websocket.TextMessage, resp)
	return role == "primary"
}

// broadcastToFollowers sends raw to every connected Follower
// and fans out frame payloads to SSE subscribers.
// Disconnected followers are cleaned up on their next read error.
func (h *TerminalHub) broadcastToFollowers(raw []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	bgCtx := context.Background()
	for c := range h.followers {
		if err := c.WriteMessage(websocket.TextMessage, raw); err != nil {
			h.ulog.Debug("Failed to write to follower").Err(err).Log(bgCtx)
			// Don't remove here — the follower's read loop will detect the
			// broken connection and clean up via removeConn.
		}
	}

	// Fan out to SSE subscribers if this is a frame or layout message.
	if len(h.sseSubscribers) > 0 {
		var msg WsMessage
		if err := json.Unmarshal(raw, &msg); err == nil && (msg.Type == "frame" || msg.Type == "layout") {
			// Extract the base64 data field from the frame payload.
			var fp struct {
				Data []byte `json:"data"`
			}
			if msg.Type == "frame" {
				if err := json.Unmarshal(msg.Payload, &fp); err != nil {
					return
				}
			}

			// Build SSE event with type prefix.
			var sseData string
			if msg.Type == "layout" {
				sseData = "event: layout\ndata: " + string(msg.Payload) + "\n\n"
			} else {
				sseData = "event: frame\ndata: " + base64.StdEncoding.EncodeToString(fp.Data) + "\n\n"
			}

			for ch := range h.sseSubscribers {
				select {
				case ch <- sseData:
				default:
					// Slow SSE client — drop the frame.
					h.ulog.Debug("Dropping frame for slow SSE subscriber").Log(bgCtx)
				}
			}
		}
	}
}

// SubscribeSSE registers an SSE channel and returns it.
// The caller must call UnsubscribeSSE when done.
func (h *TerminalHub) SubscribeSSE() chan string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan string, 64)
	h.sseSubscribers[ch] = true
	h.ulog.Info("SSE subscriber connected").Log(context.Background())
	return ch
}

// UnsubscribeSSE removes an SSE channel and closes it.
func (h *TerminalHub) UnsubscribeSSE(ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sseSubscribers, ch)
	close(ch)
	h.ulog.Info("SSE subscriber disconnected").Log(context.Background())
}

// RequestFullSync sends a "request_full_sync" message to the Primary terminal,
// asking it to produce a full-screen payload on its next compositor tick.
// Used when a new SSE client connects so it gets a complete initial frame.
func (h *TerminalHub) RequestFullSync() {
	h.mu.RLock()
	primary := h.primary
	h.mu.RUnlock()

	if primary == nil {
		return
	}

	msg, _ := json.Marshal(WsMessage{
		Type:    "request_full_sync",
		Payload: json.RawMessage("{}"),
	})
	if err := primary.WriteMessage(websocket.TextMessage, msg); err != nil {
		h.ulog.Debug("Failed to send request_full_sync to primary").Err(err).Log(context.Background())
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
		h.ulog.Debug("Failed to write to primary").Err(err).Log(context.Background())
	}
}

// removeConn cleans up a disconnected connection.
// If the Primary disconnects, all Followers are notified.
func (h *TerminalHub) removeConn(conn *websocket.Conn, isPrimary bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	bgCtx := context.Background()
	if isPrimary && h.primary == conn {
		h.primary = nil
		h.ulog.Info("Primary disconnected").Log(bgCtx)

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
		h.ulog.Debug("Follower disconnected").Log(bgCtx)
	}
}
