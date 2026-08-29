package identity

import (
	"context"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// PrincipalResolver maps a VERIFIED (issuer, subject) pair to the
// server-side PrincipalContext with project memberships. Implementations
// derive memberships from the identity store — never from request data.
type PrincipalResolver interface {
	Resolve(ctx context.Context, issuer, subject string) (*model.PrincipalContext, error)
}

// StaticResolver serves tests and the local single-tenant baseline: a
// fixed, server-configured membership map.
type StaticResolver struct {
	Memberships map[string]map[string]string // subject -> project -> role
}

// Resolve returns the configured principal for a subject, failing closed
// for unknown subjects.
func (s *StaticResolver) Resolve(_ context.Context, issuer, subject string) (*model.PrincipalContext, error) {
	if issuer == "" || subject == "" {
		return nil, fmt.Errorf("identity: issuer and subject are required")
	}
	memberships, ok := s.Memberships[subject]
	if !ok {
		return nil, fmt.Errorf("identity: unknown subject")
	}
	return &model.PrincipalContext{
		PrincipalID:        issuer + "/" + subject,
		Type:               model.PrincipalTypeHuman,
		ProjectMemberships: memberships,
	}, nil
}
