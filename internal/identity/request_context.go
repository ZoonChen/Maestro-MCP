package identity

import (
	"context"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Request-scoped principal propagation (W5-1). The identity layer
// resolves the authenticated principal once per HTTP request (bearer or
// BFF cookie). The MCP Streamable HTTP transport is served behind the
// SAME engine-wide Authenticate middleware, so the bridge below carries
// the resolved principal from the request into the context the MCP
// tool handlers run under — the guard then authorizes tool calls with
// the server-side principal (memberships + functional + platform
// roles) instead of the delegated-session synthesis. Credential
// verification itself never happens here: this file only moves an
// already-authenticated principal across one transport boundary.

type requestPrincipalContextKey struct{}

// WithRequestPrincipal attaches the authenticated principal to a
// request context. The value is always set by the server-side identity
// layer, never read from any request payload.
func WithRequestPrincipal(ctx context.Context, principal *model.PrincipalContext) context.Context {
	return context.WithValue(ctx, requestPrincipalContextKey{}, principal)
}

// RequestPrincipalFrom returns the authenticated principal carried by
// the context, or nil when the transport is not identity-bound (the
// local delegated-context runner).
func RequestPrincipalFrom(ctx context.Context) *model.PrincipalContext {
	if ctx == nil {
		return nil
	}
	principal, _ := ctx.Value(requestPrincipalContextKey{}).(*model.PrincipalContext)
	return principal
}
