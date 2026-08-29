package identity

import (
	"context"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Static evaluator for the frozen permission matrix. Dynamic conditions
// (membership freshness, subject-not-author, rate limits) are enforced by
// the calling surface with request context; this decision point owns the
// role/action/scope algebra and the fail-closed defaults.

// delegationDeniedActions are refused for delegated (agent) principals per
// the frozen delegation flags: an agent never self-reviews, waives or
// merges, and service accounts never inherit human roles.
var delegationDeniedActions = map[string]struct{}{
	"waiver.approve":          {},
	"waiver.approve.security": {},
	"waiver.approve.quality":  {},
	"verification.submit":     {},
	"protected_branch.merge":  {},
}

// Authorize is the unified policy decision point for human and delegated
// principals. The project scope comes from the SERVER-DERIVED membership
// map on the principal, never from the request; a resource outside every
// membership is denied (the transport maps that to 404, never 403).
// Caller mistakes degrade to deny decisions: this point never errors open.
func (p *Policy) Authorize(_ context.Context, principal *model.PrincipalContext, action string, resource model.Resource) model.Decision {
	if principal == nil {
		return deny(p, "no principal")
	}
	if resource.Type == "" {
		return deny(p, "invalid resource: type is required")
	}

	// Protected actions: Maestro never executes them regardless of role.
	if action == "protected_branch.merge" || action == "gitlab.merge" {
		if !p.Protected.FinalMergeAllowed {
			return deny(p, "protected action: final merge is human-only in GitLab")
		}
	}

	role, ok := principal.ProjectMemberships[resource.ProjectID]
	if !ok {
		return deny(p, fmt.Sprintf("no membership in project scope %q", resource.ProjectID))
	}
	grants, ok := p.Roles[role]
	if !ok {
		return deny(p, fmt.Sprintf("unknown role %q", role))
	}

	if _, denied := grants.Deny[action]; denied {
		return deny(p, fmt.Sprintf("role %q denies %s", role, action))
	}
	if _, allowed := grants.Allow[action]; !allowed {
		return deny(p, fmt.Sprintf("role %q does not grant %s", role, action))
	}

	// Delegated principals act at the intersection of human grants and the
	// frozen agent restrictions.
	if principal.DelegationID != "" {
		if !p.Delegation.AgentSelfReviewWaive {
			if _, denied := delegationDeniedActions[action]; denied {
				return deny(p, "delegated principals may not self-review, waive or merge")
			}
		}
	}

	return model.Decision{
		Allow:         true,
		PolicyVersion: p.Version,
		Reasons:       []string{fmt.Sprintf("role %q allows %s in project %s", role, action, resource.ProjectID)},
	}
}

// AuthorizeService evaluates a service or bootstrap identity (runner
// device, GitLab bot, background worker, enrollment code) by identity
// name. Service accounts never inherit human roles.
func (p *Policy) AuthorizeService(_ context.Context, identityName, action string, resource model.Resource) model.Decision {
	if resource.Type == "" {
		return deny(p, "invalid resource: type is required")
	}
	grants, ok := p.Service[identityName]
	if !ok {
		grants, ok = p.Bootstrap[identityName]
	}
	if !ok {
		return deny(p, fmt.Sprintf("unknown service identity %q", identityName))
	}
	if _, denied := grants.Deny[action]; denied {
		return deny(p, fmt.Sprintf("service identity %q denies %s", identityName, action))
	}
	if _, allowed := grants.Allow[action]; !allowed {
		return deny(p, fmt.Sprintf("service identity %q does not grant %s", identityName, action))
	}
	return model.Decision{
		Allow:         true,
		PolicyVersion: p.Version,
		Reasons:       []string{fmt.Sprintf("service identity %q allows %s", identityName, action)},
	}
}

// AllowFunctionalRole evaluates functional approver authorities
// (security_owner, qa_owner). Static grant check only: the frozen
// conditions (approver-not-author, membership, category match) are
// enforced by the calling surface with request context.
func (p *Policy) AllowFunctionalRole(_ context.Context, approverRole, action string) bool {
	grants, ok := p.FunctionalRole[approverRole]
	if !ok {
		return false
	}
	if _, denied := grants.Deny[action]; denied {
		return false
	}
	_, allowed := grants.Allow[action]
	return allowed
}

func deny(p *Policy, reason string) model.Decision {
	return model.Decision{
		Allow:         false,
		PolicyVersion: p.Version,
		Reasons:       []string{reason},
	}
}
