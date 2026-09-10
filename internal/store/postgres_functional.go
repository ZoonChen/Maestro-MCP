package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// J1 functional-principal store (task brief J1 / G-α): the explicit
// binding configuration for functional roles. One row is one grant of
// one function to one user, with a validity window, a terminal
// revocation and the authorization-source reference (blueprint §4.2
// 授权书 citation; validating that a resolvable deed exists is the
// registered J2a follow-up once the asset ledger lands). The table
// sits beside memberships and never feeds ProjectMemberships.

// Functional role constants (migration 0016 CHECK enum). All five are
// bindable; only security_owner and qa_owner carry frozen grants in
// permissions.yaml today — binding the other three is possible, they
// simply authorize nothing until the matrix is extended by contract.
const (
	FunctionalRoleSecurityOwner   = "security_owner"
	FunctionalRoleQAOwner         = "qa_owner"
	FunctionalRoleOperationsOwner = "operations_owner"
	FunctionalRoleProductOwner    = "product_owner"
	FunctionalRoleTechnicalLead   = "technical_lead"
)

// FunctionalPrincipal is one functional_grants row projection.
type FunctionalPrincipal struct {
	ID        string
	UserID    string
	Function  string
	ValidFrom string
	ValidTo   *string
	RevokedAt *string
	SourceRef string
	CreatedAt string
	UpdatedAt string
}

var (
	// ErrFunctionalGrantInvalid marks a rejected binding: unknown
	// function, empty user or an inverted validity window. The grant is
	// never partially applied (fail-closed).
	ErrFunctionalGrantInvalid = errors.New("functional grant is invalid")
	// ErrFunctionalGrantConflict marks a second ACTIVE grant of the same
	// function to the same user — the caller revokes or lets expire first.
	ErrFunctionalGrantConflict = errors.New("an active functional grant already exists")
	// ErrFunctionalGrantNotFound marks an unknown or already-revoked grant.
	ErrFunctionalGrantNotFound = errors.New("functional grant not found or already revoked")
)

// validFunctionalRoles is the store-side enum mirror of the 0016 CHECK.
var validFunctionalRoles = map[string]struct{}{
	FunctionalRoleSecurityOwner:   {},
	FunctionalRoleQAOwner:         {},
	FunctionalRoleOperationsOwner: {},
	FunctionalRoleProductOwner:    {},
	FunctionalRoleTechnicalLead:   {},
}

// GrantFunctionalRole creates one binding. Validation fails closed:
// unknown functions, unknown users, inverted windows and an empty
// source reference are rejected — a grant must cite its authorization
// source even before J2a can validate the deed itself. A second ACTIVE
// grant of the same function to the same user conflicts instead of
// accumulating.
func (s pgIdentityStore) GrantFunctionalRole(ctx context.Context, grant *FunctionalPrincipal) error {
	if grant == nil {
		return fmt.Errorf("%w: nil grant", ErrFunctionalGrantInvalid)
	}
	if _, ok := validFunctionalRoles[grant.Function]; !ok {
		return fmt.Errorf("%w: unknown function %q", ErrFunctionalGrantInvalid, grant.Function)
	}
	if grant.UserID == "" {
		return fmt.Errorf("%w: user is required", ErrFunctionalGrantInvalid)
	}
	if _, err := s.GetUser(ctx, grant.UserID); err != nil {
		return fmt.Errorf("%w: unknown user", ErrFunctionalGrantInvalid)
	}
	if grant.SourceRef == "" {
		return fmt.Errorf("%w: source_ref is required (authorization deed citation)", ErrFunctionalGrantInvalid)
	}
	var validTo *time.Time
	if grant.ValidTo != nil && *grant.ValidTo != "" {
		parsed, err := time.Parse(time.RFC3339, *grant.ValidTo)
		if err != nil {
			return fmt.Errorf("%w: valid_to is not RFC3339", ErrFunctionalGrantInvalid)
		}
		validTo = &parsed
	}
	// An absent valid_from defaults to the SERVER clock (SQL now()):
	// validity is judged by now() in every read, so a client-clocked
	// default could land microseconds in the server's future and make
	// a just-granted authority briefly invisible. An inverted window
	// against the server default still fails the schema CHECK.
	var validFrom any
	if grant.ValidFrom != "" {
		parsed, err := time.Parse(time.RFC3339, grant.ValidFrom)
		if err != nil {
			return fmt.Errorf("%w: valid_from is not RFC3339", ErrFunctionalGrantInvalid)
		}
		if validTo != nil && !validTo.After(parsed) {
			return fmt.Errorf("%w: valid_to must be after valid_from", ErrFunctionalGrantInvalid)
		}
		validFrom = parsed
	}

	result, err := s.q.ExecContext(ctx, `
		INSERT INTO functional_principals (id, user_id, function, valid_from, valid_to, source_ref)
		VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6)
		ON CONFLICT (user_id, function) WHERE revoked_at IS NULL DO NOTHING`,
		pgNewUUID(), pgArg(grant.UserID), grant.Function, validFrom, validTo, grant.SourceRef)
	if err != nil {
		return fmt.Errorf("identity: grant functional role: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrFunctionalGrantConflict
	}
	return nil
}

// RevokeFunctionalRole applies the terminal revocation. Unknown and
// already-revoked grants answer ErrFunctionalGrantNotFound; the next
// resolve drops the authority because validity is re-checked per
// request against the row.
func (s pgIdentityStore) RevokeFunctionalRole(ctx context.Context, grantID string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE functional_principals SET revoked_at = now(), updated_at = now()
		WHERE id = $1 AND revoked_at IS NULL`,
		pgArg(grantID))
	if err != nil {
		return fmt.Errorf("identity: revoke functional role: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrFunctionalGrantNotFound
	}
	return nil
}

// ListFunctionalPrincipals returns the bindings of one function (all
// functions when function is empty), including expired and revoked
// rows — the administration view, not the authorization view.
func (s pgIdentityStore) ListFunctionalPrincipals(ctx context.Context, function string) ([]FunctionalPrincipal, error) {
	query := `
		SELECT id, user_id, function, valid_from, valid_to, revoked_at, source_ref, created_at, updated_at
		FROM functional_principals`
	args := []any{}
	if function != "" {
		query += ` WHERE function = $1`
		args = append(args, function)
	}
	query += ` ORDER BY created_at`
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("identity: list functional principals: %w", err)
	}
	defer rows.Close()
	grants := []FunctionalPrincipal{}
	for rows.Next() {
		grant, scanErr := scanFunctionalPrincipal(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ActiveFunctionalRoles returns the functions the user currently
// holds: unrevoked and inside their validity window at read time.
// This is the resolver's authorization view — expired or revoked
// grants simply disappear from the list.
func (s pgIdentityStore) ActiveFunctionalRoles(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT function FROM functional_principals
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND valid_from <= now()
		  AND (valid_to IS NULL OR valid_to > now())
		ORDER BY function`,
		pgArg(userID))
	if err != nil {
		return nil, fmt.Errorf("identity: active functional roles: %w", err)
	}
	defer rows.Close()
	roles := []string{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("identity: scan functional role: %w", err)
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func scanFunctionalPrincipal(row interface{ Scan(...any) error }) (FunctionalPrincipal, error) {
	var grant FunctionalPrincipal
	var validFrom, createdAt, updatedAt time.Time
	var validTo, revokedAt sql.NullTime
	if err := row.Scan(&grant.ID, &grant.UserID, &grant.Function, &validFrom, &validTo, &revokedAt,
		&grant.SourceRef, &createdAt, &updatedAt); err != nil {
		return grant, fmt.Errorf("identity: scan functional principal: %w", err)
	}
	grant.ID = pgStr(grant.ID)
	grant.UserID = pgStr(grant.UserID)
	grant.ValidFrom = pgTimeString(validFrom)
	if validTo.Valid {
		valid := pgTimeString(validTo.Time)
		grant.ValidTo = &valid
	}
	if revokedAt.Valid {
		revoked := pgTimeString(revokedAt.Time)
		grant.RevokedAt = &revoked
	}
	grant.CreatedAt = pgTimeString(createdAt)
	grant.UpdatedAt = pgTimeString(updatedAt)
	return grant, nil
}
