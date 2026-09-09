package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Server-side BFF browser sessions (task brief E, M4-UI-001 UI-AUTH).
// The cookie carries a 32-byte random token; only its sha256 hash is
// persisted (SEC-IDENTITY-RBAC section 9). Session creation and
// revocation each append their audit row in the SAME transaction as the
// state change (state change + audit atomic).

// AuthLoginRequest is one in-flight OIDC authorization-code handshake.
// StateTokenHash is the sha256 of the opaque state token sent to the
// IdP; the row is single-use and short-lived.
type AuthLoginRequest struct {
	ID             string
	StateTokenHash []byte
	ClientState    string
	RedirectURI    string
	CallbackURI    string
	CodeVerifier   string
	ExpiresAt      time.Time
}

// AuthSession is one opaque server-side session. Revoked/Expiry state
// is evaluated by the caller at read time so expired and revoked
// sessions are observable in tests, never silently hidden.
type AuthSession struct {
	ID        string
	UserID    string
	TokenHash []byte
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// AuthSessionsStore is the persistence contract for the /auth protocol
// endpoints and the cookie authentication path.
type AuthSessionsStore interface {
	// CreateLoginRequest persists one handshake row.
	CreateLoginRequest(ctx context.Context, request AuthLoginRequest) error

	// ConsumeLoginRequest atomically burns the state token and returns
	// its handshake data; found is false for unknown, expired or
	// already-consumed tokens (a replayed callback fails closed).
	ConsumeLoginRequest(ctx context.Context, stateTokenHash []byte) (*AuthLoginRequest, bool, error)

	// CreateSession persists a new session with its creation audit row
	// in one transaction.
	CreateSession(ctx context.Context, session AuthSession, correlationID string) error

	// SessionByTokenHash resolves a session by cookie token hash,
	// independent of its validity state.
	SessionByTokenHash(ctx context.Context, tokenHash []byte) (*AuthSession, bool, error)

	// TouchSession refreshes last_seen_at on a validated session.
	TouchSession(ctx context.Context, tokenHash []byte) error

	// RevokeSessionByTokenHash terminally revokes a session with its
	// audit row in one transaction; the return reports whether an
	// active session was revoked (idempotent for unknown/revoked).
	RevokeSessionByTokenHash(ctx context.Context, tokenHash []byte, actor, correlationID string) (bool, error)
}

type pgAuthSessionStore struct{ db *sql.DB }

// AuthSessions returns the browser-session store bound to the pool.
func (s *PostgresStore) AuthSessions() AuthSessionsStore {
	return pgAuthSessionStore{db: s.db}
}

func (s pgAuthSessionStore) CreateLoginRequest(ctx context.Context, request AuthLoginRequest) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO auth_login_requests
			(id, state_token_hash, client_state, redirect_uri, callback_uri, code_verifier, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		request.ID, request.StateTokenHash, request.ClientState, request.RedirectURI,
		request.CallbackURI, request.CodeVerifier, request.ExpiresAt)
	if err != nil {
		return fmt.Errorf("auth session: create login request: %w", err)
	}
	return nil
}

func (s pgAuthSessionStore) ConsumeLoginRequest(ctx context.Context, stateTokenHash []byte) (*AuthLoginRequest, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE auth_login_requests SET consumed_at = now()
		WHERE id = (
			SELECT id FROM auth_login_requests
			WHERE state_token_hash = $1 AND consumed_at IS NULL AND expires_at > now()
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, client_state, redirect_uri, callback_uri, code_verifier, expires_at`,
		stateTokenHash)
	var request AuthLoginRequest
	err := row.Scan(&request.ID, &request.ClientState, &request.RedirectURI,
		&request.CallbackURI, &request.CodeVerifier, &request.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("auth session: consume login request: %w", err)
	}
	request.StateTokenHash = stateTokenHash
	return &request, true, nil
}

func (s pgAuthSessionStore) CreateSession(ctx context.Context, session AuthSession, correlationID string) error {
	tx, err := beginAuthTx(ctx, s.db)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_sessions (id, user_id, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`,
		session.ID, session.UserID, session.TokenHash, session.ExpiresAt); err != nil {
		return fmt.Errorf("auth session: create: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, NULL, 'auth.session.created', 'auth_session', $2, 'allow', 'oidc authorization code + pkce login', $3)`,
		session.UserID, session.ID, correlationID); err != nil {
		return fmt.Errorf("auth session: create audit: %w", err)
	}
	return tx.Commit()
}

func (s pgAuthSessionStore) SessionByTokenHash(ctx context.Context, tokenHash []byte) (*AuthSession, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, created_at, expires_at, revoked_at
		FROM auth_sessions WHERE token_hash = $1`, tokenHash)
	var session AuthSession
	var revokedAt sql.NullTime
	err := row.Scan(&session.ID, &session.UserID, &session.CreatedAt, &session.ExpiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("auth session: lookup: %w", err)
	}
	session.TokenHash = tokenHash
	if revokedAt.Valid {
		revoked := revokedAt.Time
		session.RevokedAt = &revoked
	}
	return &session, true, nil
}

func (s pgAuthSessionStore) TouchSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE auth_sessions SET last_seen_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`, tokenHash)
	if err != nil {
		return fmt.Errorf("auth session: touch: %w", err)
	}
	return nil
}

func (s pgAuthSessionStore) RevokeSessionByTokenHash(ctx context.Context, tokenHash []byte, actor, correlationID string) (bool, error) {
	tx, err := beginAuthTx(ctx, s.db)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var sessionID string
	err = tx.QueryRowContext(ctx, `
		UPDATE auth_sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
		RETURNING id`, tokenHash).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth session: revoke: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events
			(actor_principal, project_id, action, resource_type, resource_id, decision, reason, correlation_id)
		VALUES ($1, NULL, 'auth.session.revoked', 'auth_session', $2, 'allow', 'user logout', $3)`,
		actor, sessionID, correlationID); err != nil {
		return false, fmt.Errorf("auth session: revoke audit: %w", err)
	}
	return true, tx.Commit()
}

// beginAuthTx adapts the pool-bound and transaction-bound stores; the
// session writes are standalone, so a fresh transaction is always opened.
// beginAuthTx opens the transaction for one audited session write.
func beginAuthTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("auth session: begin: %w", err)
	}
	return tx, nil
}

// Compile-time contract assertion.
var _ AuthSessionsStore = pgAuthSessionStore{}
