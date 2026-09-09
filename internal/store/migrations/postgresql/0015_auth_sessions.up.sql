-- M4 browser sessions (P4, session E; task brief E / M4-UI-001 UI-AUTH).
--
-- Authority: docs/security/identity-rbac.md section 2/7 (Browser: OIDC
-- Authorization Code + PKCE, BFF secure cookie) and section 9 (only
-- token-ID/JTI hashes at rest). Two tables:
--
--   auth_login_requests: the server side of the OIDC authorization-code
--     handshake. The opaque state token sent to the IdP is stored as a
--     sha256 hash; the row is single-use (consumed_at) and short-lived
--     (expires_at), so replayed or stale callbacks fail closed.
--
--   auth_sessions: server-side opaque BFF sessions. The cookie value is
--     a 32-byte random token; only its sha256 hash is persisted. A
--     session is revoked by setting revoked_at (revocation propagates
--     on the next request because validity is re-checked per request
--     against this row), never by deleting history.

CREATE TABLE auth_login_requests (
    id              uuid PRIMARY KEY,
    state_token_hash bytea NOT NULL UNIQUE,
    client_state    text NOT NULL DEFAULT '',
    redirect_uri    text NOT NULL,
    callback_uri    text NOT NULL,
    code_verifier   text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    consumed_at     timestamptz,
    -- A login request is consumable exactly once: the guarded UPDATE in
    -- the store is the one-shot state/CSRF boundary of the flow.
    CHECK (consumed_at IS NULL OR consumed_at >= created_at)
);

CREATE INDEX idx_auth_login_requests_expiry ON auth_login_requests (expires_at);

CREATE TABLE auth_sessions (
    id          uuid PRIMARY KEY,
    user_id     uuid NOT NULL,
    token_hash  bytea NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    last_seen_at timestamptz,
    revoked_at  timestamptz,
    CHECK (expires_at > created_at),
    -- Revocation is terminal; expiry is wall-clock checked at read time.
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE INDEX idx_auth_sessions_user ON auth_sessions (user_id);
