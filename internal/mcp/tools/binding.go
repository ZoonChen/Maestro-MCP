package tools

import "errors"

// TransportBinding is the server-side scope and identity assigned to an MCP
// transport at composition time (M1-MCP-001 / SEC-IDENTITY-RBAC section 2).
// For the local stdio Runner it is the single-user, single-project delegated
// context injected by the host startup configuration; for the Streamable
// HTTP transport the identity layer (M1-AUTH-001) resolves it per
// connection. Request payloads can never override any of these fields.
type TransportBinding struct {
	ProjectID string
	SessionID string
	WorkerID  string
}

var errUnboundTransport = errors.New("transport scope is not bound; start the Runner with an explicit server-side project binding")

// scope resolves the binding or fails closed: an unbound transport never
// gets a default project or synthetic session identity.
func (b *TransportBinding) scope() (projectID, sessionID, workerID string, err error) {
	if b == nil || b.ProjectID == "" || b.SessionID == "" || b.WorkerID == "" {
		return "", "", "", errUnboundTransport
	}
	return b.ProjectID, b.SessionID, b.WorkerID, nil
}
