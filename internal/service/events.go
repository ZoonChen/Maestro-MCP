package service

// EventEmitter defines the interface for broadcasting real-time events to
// WebSocket-connected dashboard clients. Services use this to push state
// change notifications without depending directly on the ws package.
type EventEmitter interface {
	EmitEvent(eventType, projectID string, payload interface{})
}

// noopEmitter is a no-op implementation used when no WebSocket hub is available.
type noopEmitter struct{}

func (n *noopEmitter) EmitEvent(string, string, interface{}) {}

// safeEmit calls EmitEvent on the emitter if it is non-nil.
func safeEmit(em EventEmitter, eventType, projectID string, payload interface{}) {
	if em != nil {
		em.EmitEvent(eventType, projectID, payload)
	}
}
