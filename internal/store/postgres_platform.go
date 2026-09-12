package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// J5 platform-grant store (task brief J5 / CR-P5a-1): the explicit
// binding configuration for platform roles. One row is one grant of
// one platform role to one user, with a validity window, a terminal
// revocation and the authorization-source reference (blueprint §4.2
// 授权书 citation, same discipline as functional_principals). The
// table sits beside memberships and functional_principals and never
// feeds ProjectMemberships or FunctionalRoles.

// Platform role constants (migration 0020 CHECK enum). platform_admin
// is the only frozen platform role today; the enum grows by migration
// with the frozen matrix, never by drift.
const (
	PlatformRoleAdmin = "platform_admin"
)

// PlatformGrant is one platform_grants row projection.
type PlatformGrant struct {
	ID        string
	UserID    string
	Role      string
	ValidFrom string
	ValidTo   *string
	RevokedAt *string
	SourceRef string
	CreatedAt string
	UpdatedAt string
}

var (
	// ErrPlatformGrantInvalid marks a rejected binding: unknown role,
	// empty user or an inverted validity window. The grant is never
	// partially applied (fail-closed).
	ErrPlatformGrantInvalid = errors.New("platform grant is invalid")
	// ErrPlatformGrantConflict marks a second ACTIVE grant of the same
	// platform role to the same user — the caller revokes or lets
	// expire first.
	ErrPlatformGrantConflict = errors.New("an active platform grant already exists")
	// ErrPlatformGrantNotFound marks an unknown or already-revoked grant.
	ErrPlatformGrantNotFound = errors.New("platform grant not found or already revoked")
)

// validPlatformRoles is the store-side enum mirror of the 0020 CHECK.
var validPlatformRoles = map[string]struct{}{
	PlatformRoleAdmin: {},
}

// GrantPlatformRole creates one binding. Validation fails closed:
// unknown roles, unknown users, inverted windows and an empty source
// reference are rejected — a platform grant must cite its
// authorization source (the 授权书 citation of blueprint §4.2, same
// requirement as functional grants). A second ACTIVE grant of the same
// role to the same user conflicts instead of accumulating.
func (s pgIdentityStore) GrantPlatformRole(ctx context.Context, grant *PlatformGrant) error {
	if grant == nil {
		return fmt.Errorf("%w: nil grant", ErrPlatformGrantInvalid)
	}
	if _, ok := validPlatformRoles[grant.Role]; !ok {
		return fmt.Errorf("%w: unknown platform role %q", ErrPlatformGrantInvalid, grant.Role)
	}
	validFrom, validTo, err := s.validateGrantBinding(ctx, grantBinding{
		UserID: grant.UserID, SourceRef: grant.SourceRef,
		ValidFrom: grant.ValidFrom, ValidTo: grant.ValidTo,
	}, ErrPlatformGrantInvalid)
	if err != nil {
		return err
	}

	result, err := s.q.ExecContext(ctx, `
		INSERT INTO platform_grants (id, user_id, role, valid_from, valid_to, source_ref)
		VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6)
		ON CONFLICT (user_id, role) WHERE revoked_at IS NULL DO NOTHING`,
		pgNewUUID(), pgArg(grant.UserID), grant.Role, validFrom, validTo, grant.SourceRef)
	if err != nil {
		return fmt.Errorf("identity: grant platform role: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrPlatformGrantConflict
	}
	return nil
}

// RevokePlatformRole applies the terminal revocation. Unknown and
// already-revoked grants answer ErrPlatformGrantNotFound; the next
// resolve drops the authority because validity is re-checked per
// request against the row.
func (s pgIdentityStore) RevokePlatformRole(ctx context.Context, grantID string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE platform_grants SET revoked_at = now(), updated_at = now()
		WHERE id = $1 AND revoked_at IS NULL`,
		pgArg(grantID))
	if err != nil {
		return fmt.Errorf("identity: revoke platform role: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrPlatformGrantNotFound
	}
	return nil
}

// ListPlatformGrants returns the bindings of one platform role (all
// roles when role is empty), including expired and revoked rows — the
// administration view, not the authorization view.
func (s pgIdentityStore) ListPlatformGrants(ctx context.Context, role string) ([]PlatformGrant, error) {
	query := `
		SELECT id, user_id, role, valid_from, valid_to, revoked_at, source_ref, created_at, updated_at
		FROM platform_grants`
	args := []any{}
	if role != "" {
		query += ` WHERE role = $1`
		args = append(args, role)
	}
	query += ` ORDER BY created_at`
	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("identity: list platform grants: %w", err)
	}
	defer rows.Close()
	grants := []PlatformGrant{}
	for rows.Next() {
		grant, scanErr := scanPlatformGrant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

// ActivePlatformRoles returns the platform roles the user currently
// holds: unrevoked and inside their validity window at read time.
// This is the resolver's authorization view — expired or revoked
// grants simply disappear from the list.
func (s pgIdentityStore) ActivePlatformRoles(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT role FROM platform_grants
		WHERE user_id = $1
		  AND revoked_at IS NULL
		  AND valid_from <= now()
		  AND (valid_to IS NULL OR valid_to > now())
		ORDER BY role`,
		pgArg(userID))
	if err != nil {
		return nil, fmt.Errorf("identity: active platform roles: %w", err)
	}
	defer rows.Close()
	roles := []string{}
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("identity: scan platform role: %w", err)
		}
		roles = append(roles, role)
	}
	return roles, rows.Err()
}

func scanPlatformGrant(row interface{ Scan(...any) error }) (PlatformGrant, error) {
	var grant PlatformGrant
	var validFrom, createdAt, updatedAt time.Time
	var validTo, revokedAt sql.NullTime
	if err := row.Scan(&grant.ID, &grant.UserID, &grant.Role, &validFrom, &validTo, &revokedAt,
		&grant.SourceRef, &createdAt, &updatedAt); err != nil {
		return grant, fmt.Errorf("identity: scan platform grant: %w", err)
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
