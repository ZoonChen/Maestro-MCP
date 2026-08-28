package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// PostgreSQL implementation of IdentityStore (ADR-003): OIDC-backed users
// and team memberships. Only non-sensitive claims are persisted; tokens and
// secrets never reach this layer.

const pgUserColumns = `id, issuer, subject, display_name, status, created_at, updated_at`

func scanPGUser(row interface{ Scan(...any) error }) (*model.User, error) {
	var user model.User
	var createdAt, updatedAt time.Time
	if err := row.Scan(&user.ID, &user.Issuer, &user.Subject, &user.DisplayName,
		&user.Status, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	user.ID = pgStr(user.ID)
	user.CreatedAt = pgTimeString(createdAt)
	user.UpdatedAt = pgTimeString(updatedAt)
	return &user, nil
}

// pgExecer abstracts *sql.DB and *sql.Tx so the same store implementation
// serves base calls and UnitOfWork transactions.
type pgExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type pgIdentityStore struct{ q pgExecer }

// GetOrCreateUser maps a verified issuer+subject pair to exactly one user
// row; repeated logins refresh the display name idempotently.
func (s pgIdentityStore) GetOrCreateUser(ctx context.Context, issuer, subject, displayName string) (*model.User, error) {
	row := s.q.QueryRowContext(ctx, `
		INSERT INTO users (id, issuer, subject, display_name, status)
		VALUES ($1, $2, $3, $4, 'active')
		ON CONFLICT (issuer, subject) DO UPDATE SET display_name = EXCLUDED.display_name
		RETURNING `+pgUserColumns,
		pgNewUUID(), issuer, subject, displayName)
	user, err := scanPGUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("identity: user upsert returned no row")
	}
	if err != nil {
		return nil, fmt.Errorf("identity: upsert user: %w", err)
	}
	return user, nil
}

func (s pgIdentityStore) GetUser(ctx context.Context, id string) (*model.User, error) {
	row := s.q.QueryRowContext(ctx, `SELECT `+pgUserColumns+` FROM users WHERE id = $1`, pgArg(id))
	user, err := scanPGUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("identity: get user: %w", err)
	}
	return user, nil
}

func (s pgIdentityStore) UpdateUserStatus(ctx context.Context, id, expectedStatus, newStatus string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE users SET status = $3, updated_at = now()
		WHERE id = $1 AND status = $2`,
		pgArg(id), expectedStatus, newStatus)
	if err != nil {
		return fmt.Errorf("identity: update user status: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (s pgIdentityStore) CreateMembership(ctx context.Context, m *model.TeamMembership) error {
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO memberships (team_id, user_id, role, valid_from, valid_to)
		VALUES ($1, $2, $3, COALESCE($4, now()), $5)
		ON CONFLICT (team_id, user_id) DO NOTHING`,
		pgArg(m.TeamID), pgArg(m.UserID), m.Role, pgOptionalTime(m.ValidFrom), pgOptionalTimePtr(m.ValidTo))
	if err != nil {
		return fmt.Errorf("identity: create membership: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrMembershipNotFound
	}
	return nil
}

func (s pgIdentityStore) ListMembershipsByUser(ctx context.Context, userID, at string) ([]*model.TeamMembership, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT team_id, user_id, role, valid_from, valid_to, created_at, updated_at
		FROM memberships
		WHERE user_id = $1
		  AND valid_from <= COALESCE($2::timestamptz, now())
		  AND (valid_to IS NULL OR valid_to > COALESCE($2::timestamptz, now()))
		ORDER BY team_id`,
		pgArg(userID), pgOptionalTime(at))
	if err != nil {
		return nil, fmt.Errorf("identity: list memberships: %w", err)
	}
	defer rows.Close()
	var memberships []*model.TeamMembership
	for rows.Next() {
		m := &model.TeamMembership{}
		var validFrom, createdAt, updatedAt time.Time
		var validTo sql.NullTime
		if err := rows.Scan(&m.TeamID, &m.UserID, &m.Role, &validFrom, &validTo, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("identity: scan membership: %w", err)
		}
		m.TeamID = pgStr(m.TeamID)
		m.UserID = pgStr(m.UserID)
		m.ValidFrom = pgTimeString(validFrom)
		if validTo.Valid {
			valid := pgTimeString(validTo.Time)
			m.ValidTo = &valid
		}
		m.CreatedAt = pgTimeString(createdAt)
		m.UpdatedAt = pgTimeString(updatedAt)
		memberships = append(memberships, m)
	}
	return memberships, rows.Err()
}

func (s pgIdentityStore) ListProjectMemberships(ctx context.Context, userID string) ([]ProjectMembershipView, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT p.id, m.role
		FROM memberships m
		JOIN projects p ON p.team_id = m.team_id
		WHERE m.user_id = $1
		  AND m.valid_from <= now()
		  AND (m.valid_to IS NULL OR m.valid_to > now())
		ORDER BY p.id`,
		pgArg(userID))
	if err != nil {
		return nil, fmt.Errorf("identity: derive project memberships: %w", err)
	}
	defer rows.Close()
	views := []ProjectMembershipView{}
	for rows.Next() {
		var view ProjectMembershipView
		var projectID string
		if err := rows.Scan(&projectID, &view.Role); err != nil {
			return nil, fmt.Errorf("identity: scan project membership: %w", err)
		}
		view.ProjectID = pgStr(projectID)
		views = append(views, view)
	}
	return views, rows.Err()
}
