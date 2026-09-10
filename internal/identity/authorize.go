package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Static evaluator for the frozen permission matrix. Dynamic conditions
// (membership freshness, subject-not-author, rate limits) are enforced by
// the calling surface with request context; this decision point owns the
// role/action/scope algebra and the fail-closed defaults.

// delegationDeniedActions are refused for delegated (agent) principals per
// the frozen delegation flags: an agent never self-reviews, waives or
// merges, and service accounts never inherit human roles. asset.approve
// rides the same veto (J4): the asset release is a human functional-owner
// action; agents may still register and review assets.
var delegationDeniedActions = map[string]struct{}{
	"waiver.approve":          {},
	"waiver.approve.security": {},
	"waiver.approve.quality":  {},
	"asset.approve":           {},
	"verification.submit":     {},
	"protected_branch.merge":  {},
}

// Authorize is the unified policy decision point for human and delegated
// principals. The project scope comes from the SERVER-DERIVED membership
// map on the principal, never from the request; a resource outside every
// membership is denied (the transport maps that to 404, never 403).
// Caller mistakes degrade to deny decisions: this point never errors open.
//
// Two grant classes merge (J1): the project-role grants from the
// membership map and the frozen functional_approvers grants from the
// principal's ACTIVE functional roles. Each class is evaluated on its
// own allow/deny algebra — a functional grant confers ONLY the frozen
// functional permissions and never stacks project permissions, and a
// project role's deny set constrains the project path, not the
// functional authority the organization granted separately (the frozen
// separation-of-duties conditions — membership presence, approver ≠
// requester — still gate the waiver actions). Both classes require an
// active membership in the project scope (the frozen
// project_membership_required condition); the delegated-principal
// restriction vetoes both.
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

	// Delegated principals act at the intersection of human grants and
	// the frozen agent restrictions; the veto applies to every grant
	// class (an agent never self-reviews, waives or merges).
	if principal.DelegationID != "" && !p.Delegation.AgentSelfReviewWaive {
		if _, denied := delegationDeniedActions[action]; denied {
			return deny(p, "delegated principals may not self-review, waive or merge")
		}
	}

	reasons := []string{}

	// Project-role grant path.
	grants, known := p.Roles[role]
	switch {
	case !known:
		reasons = append(reasons, fmt.Sprintf("unknown role %q", role))
	default:
		if _, denied := grants.Deny[action]; denied {
			reasons = append(reasons, fmt.Sprintf("role %q denies %s", role, action))
		} else if _, allowed := grants.Allow[action]; allowed {
			return model.Decision{
				Allow:         true,
				PolicyVersion: p.Version,
				Reasons:       []string{fmt.Sprintf("role %q allows %s in project %s", role, action, resource.ProjectID)},
			}
		} else {
			reasons = append(reasons, fmt.Sprintf("role %q does not grant %s", role, action))
		}
	}

	// Functional-role grant path: frozen functional permissions only.
	for _, functional := range principal.FunctionalRoles {
		grants, known := p.FunctionalRole[functional]
		if !known {
			reasons = append(reasons, fmt.Sprintf("unknown functional role %q", functional))
			continue
		}
		if _, denied := grants.Deny[action]; denied {
			reasons = append(reasons, fmt.Sprintf("functional role %q denies %s", functional, action))
			continue
		}
		if _, allowed := grants.Allow[action]; allowed {
			return model.Decision{
				Allow:         true,
				PolicyVersion: p.Version,
				Reasons:       []string{fmt.Sprintf("functional role %q allows %s in project %s", functional, action, resource.ProjectID)},
			}
		}
		reasons = append(reasons, fmt.Sprintf("functional role %q does not grant %s", functional, action))
	}

	return deny(p, strings.Join(reasons, "; "))
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
// (security_owner, qa_owner and the J4 product_owner, technical_lead,
// operations_owner planes). Static grant check only: the frozen
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

// Authority reports which grant class produced an allow decision, for
// the audit subject distinction (J1-4): "functional:security_owner" or
// "project:viewer". It reads the canonical allow reason written by
// Authorize in this same package; a deny or unrecognized shape reports
// "" and the caller must not treat it as an authority claim.
func Authority(decision model.Decision) string {
	if !decision.Allow || len(decision.Reasons) == 0 {
		return ""
	}
	reason := decision.Reasons[0]
	switch {
	case strings.HasPrefix(reason, functionalAllowPrefix):
		role := strings.TrimPrefix(reason, functionalAllowPrefix)
		if idx := strings.Index(role, `" allows `); idx >= 0 {
			return "functional:" + role[:idx]
		}
	case strings.HasPrefix(reason, projectAllowPrefix):
		role := strings.TrimPrefix(reason, projectAllowPrefix)
		if idx := strings.Index(role, `" allows `); idx >= 0 {
			return "project:" + role[:idx]
		}
	}
	return ""
}

const (
	functionalAllowPrefix = `functional role "`
	projectAllowPrefix    = `role "`
)
