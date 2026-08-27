// Package ws implements a simple WebSocket hub for broadcasting real-time events
// to connected dashboard clients. Clients can subscribe to specific project IDs
// to receive only relevant events.
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait is the time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// pongWait is the time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// pingPeriod is how often pings are sent to the peer. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// maxMessageSize is the maximum message size allowed from the peer.
	maxMessageSize = 512
)

// ---------------------------------------------------------------------------
// Event types — used by the service layer to emit typed WebSocket events.
// Each constant maps to a JSON "type" field in the event envelope.
// ---------------------------------------------------------------------------

const (
	// Task lifecycle events
	EventTaskCreated         = "task.created"
	EventTaskClaimed         = "task.claimed"
	EventTaskSubmitted       = "task.submitted"
	EventTaskApproved        = "task.approved"
	EventTaskRejected        = "task.rejected"
	EventTaskBlocked         = "task.blocked"
	EventTaskUnblocked       = "task.unblocked"
	EventTaskVerifying       = "task.verifying"
	EventTaskMergeConflicted = "task.merge_conflicted"
	EventTaskMerged          = "task.merged"
	EventTaskMergeRequested  = "task.merge_requested"
	EventTaskReopened        = "task.reopened"
	EventTaskCancelled       = "task.cancelled"
	EventTaskDone            = "task.done"

	// Session events
	EventSessionOnline  = "session.online"
	EventSessionOffline = "session.offline"

	// Feature events
	EventFeatureCreated = "feature.created"
	EventFeatureUpdated = "feature.updated"

	// Validation events
	EventValidationStarted = "validation.started"
	EventValidationPassed  = "validation.passed"
	EventValidationFailed  = "validation.failed"
)

// Event represents a typed WebSocket event with a standard envelope.
type Event struct {
	Type      string      `json:"type"`
	ProjectID string      `json:"project_id"`
	Payload   interface{} `json:"payload"`
	Timestamp string      `json:"timestamp"`
}

// NewEvent creates a new typed event with the current timestamp.
func NewEvent(eventType, projectID string, payload interface{}) Event {
	return Event{
		Type:      eventType,
		ProjectID: projectID,
		Payload:   payload,
		Timestamp: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// EmitEvent marshals an event and broadcasts it to all clients subscribed
// to the given project.
func (h *Hub) EmitEvent(eventType, projectID string, payload interface{}) {
	evt := NewEvent(eventType, projectID, payload)
	data, err := json.Marshal(evt)
	if err != nil {
		slog.Error("ws hub: failed to marshal event", "event_type", eventType, "error", err)
		return
	}
	h.BroadcastProject(projectID, data)
}

// projectBroadcast is an internal message for project-scoped broadcasting.
type projectBroadcast struct {
	projectID string
	event     []byte
}

// Hub maintains the set of active WebSocket clients and broadcasts events to them.
type Hub struct {
	// clients holds all registered clients.
	clients map[*Client]bool

	// broadcast is the inbound channel for messages to be sent to all clients.
	broadcast chan []byte

	// broadcastProject is the inbound channel for project-scoped messages.
	// All map access happens inside the Run goroutine to avoid data races.
	broadcastProject chan projectBroadcast

	// register is the inbound channel for new client registrations.
	register chan *Client

	// unregister is the inbound channel for client disconnections.
	unregister chan *Client

	// stopped is closed after the event loop has disconnected all clients.
	// It prevents producers from blocking during runtime shutdown.
	stopped     chan struct{}
	stoppedOnce sync.Once
}

// Client represents a single WebSocket connection.
type Client struct {
	// Hub is the hub this client is registered with.
	Hub *Hub

	// Conn is the underlying WebSocket connection.
	Conn *websocket.Conn

	// Send is a buffered channel for outbound messages.
	Send chan []byte

	// Filters is a set of project IDs this client is interested in.
	// If empty, the client receives all events.
	Filters map[string]bool
}

// NewHub creates a new Hub instance with initialized channels.
func NewHub() *Hub {
	return &Hub{
		broadcast:        make(chan []byte, 256),
		broadcastProject: make(chan projectBroadcast, 256),
		register:         make(chan *Client),
		unregister:       make(chan *Client),
		clients:          make(map[*Client]bool),
		stopped:          make(chan struct{}),
	}
}

// Run starts the hub's main event loop. It should be launched as a goroutine.
// It handles four types of events:
//   - register: adds a new client to the active set
//   - unregister: removes a client and closes its connection
//   - broadcast: sends a message to all active clients
//   - broadcastProject: sends a message to clients filtered by project subscription
func (h *Hub) Run() {
	h.RunContext(context.Background())
}

// RunContext starts the hub event loop and drains all clients when ctx is
// cancelled. The method is synchronous so the composition root can account for
// it in its WaitGroup.
func (h *Hub) RunContext(ctx context.Context) {
	defer h.stoppedOnce.Do(func() { close(h.stopped) })
	for {
		select {
		case <-ctx.Done():
			for client := range h.clients {
				delete(h.clients, client)
				close(client.Send)
				_ = client.Conn.Close()
			}
			return
		case client := <-h.register:
			h.clients[client] = true
			slog.Info("ws hub: client registered", "total_clients", len(h.clients))

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.Send)
			}
			slog.Info("ws hub: client unregistered", "total_clients", len(h.clients))

		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.Send <- message:
				default:
					// Send buffer is full; disconnect the slow client.
					close(client.Send)
					delete(h.clients, client)
				}
			}

		case pb := <-h.broadcastProject:
			for client := range h.clients {
				if len(client.Filters) == 0 || client.Filters[pb.projectID] {
					select {
					case client.Send <- pb.event:
					default:
						close(client.Send)
						delete(h.clients, client)
					}
				}
			}
		}
	}
}

// RegisterClient enqueues a client for registration with the hub.
func (h *Hub) RegisterClient(c *Client) {
	select {
	case h.register <- c:
	case <-h.stopped:
		close(c.Send)
		_ = c.Conn.Close()
	}
}

// UnregisterClient enqueues a client for unregistration from the hub.
func (h *Hub) UnregisterClient(c *Client) {
	select {
	case h.unregister <- c:
	case <-h.stopped:
	}
}

// Broadcast sends a message to all connected clients.
func (h *Hub) Broadcast(event []byte) {
	select {
	case h.broadcast <- event:
	case <-h.stopped:
	}
}

// BroadcastProject sends a message only to clients that have subscribed to
// the given projectID. Clients with empty filters receive all messages.
// All map access is delegated to the Run goroutine via a channel to avoid data races.
func (h *Hub) BroadcastProject(projectID string, event []byte) {
	select {
	case h.broadcastProject <- projectBroadcast{projectID: projectID, event: event}:
	case <-h.stopped:
	}
}

// ReadPump reads messages from the WebSocket connection.
// It implements the client-side of the ping/pong protocol and detects
// disconnections. Incoming messages from clients are currently discarded
// (the hub is broadcast-only).
func (c *Client) ReadPump() {
	defer func() {
		c.Hub.UnregisterClient(c)
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		_ = c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("ws client read error", "error", err)
			}
			break
		}
	}
}

// WritePump writes messages from the hub to the WebSocket connection.
// It handles the server-side ping/pong protocol to keep the connection alive.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				_ = c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			_, _ = w.Write(message)

			// Drain any queued messages to batch them into the same frame.
			n := len(c.Send)
			for range n {
				_, _ = w.Write([]byte{'\n'})
				_, _ = w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			_ = c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
