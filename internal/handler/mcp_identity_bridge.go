package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ZoonChen/Maestro-MCP/internal/identity"
)

// MCP transport identity bridge (W5-1). The Streamable HTTP /mcp route
// is mounted behind the engine-wide Authenticate middleware, so every
// MCP connection already carries a server-resolved principal in the
// gin context. The bridge below moves that principal into the plain
// request context (BridgeMCPTransport) and from there into the context
// the MCP server hands to tool handlers (MCPRequestContext wired as
// the streamable HTTPContextFunc) — functional principals then reach
// asset_review / asset_approve through the SAME frozen policy REST
// uses, instead of the delegated-session role synthesis that can never
// carry functional authority.

// BridgeMCPTransport serves one MCP HTTP request with the
// authenticated principal propagated into the request context.
func BridgeMCPTransport(handler http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if principal := PrincipalFromContext(c); principal != nil {
			c.Request = c.Request.WithContext(identity.WithRequestPrincipal(c.Request.Context(), principal))
		}
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

// MCPRequestContext is the streamable HTTPContextFunc: it carries the
// request-context principal into the per-request context the MCP
// server hands to tool handlers. Transports without an identity
// principal (the local delegated-context runner) pass through
// unchanged.
func MCPRequestContext(ctx context.Context, r *http.Request) context.Context {
	principal := identity.RequestPrincipalFrom(r.Context())
	if principal == nil {
		return ctx
	}
	return identity.WithRequestPrincipal(ctx, principal)
}
