package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
)

// PostgreSQL implementation of the webhook receive/dispatch contract
// (M2-WHK-001). Every write is transactional per delivery: the inbox row
// and its append-only audit entry land together, and settle transitions
// carry their audit entry in the same transaction. The store always works
// off the pool: claim commits immediately, and BeginApply owns the settle
// transaction, so no caller-supplied transaction is required.

// staleClaimAge bounds how long a crashed dispatcher's claim survives
// before another worker reclaims the row (mirrors the outbox lease).
const staleClaimAge = 5 * time.Minute

// ErrWebhookClaimMismatch reports a settle whose lease was already taken
// over by another owner (stale reclaim); the row is not stranded — the
// new owner drives it.
var ErrWebhookClaimMismatch = errors.New("webhook inbox claim no longer owned by this dispatcher")

type pgWebhookStore struct{ db *sql.DB }

// Webhooks returns the GitLab webhook inbox store.
func (s *PostgresStore) Webhooks() webhook.Store { return pgWebhookStore{db: s.DB()} }

func (s pgWebhookStore) InstanceByID(ctx context.Context, instanceID string) (webhook.Instance, bool, error) {
	var instance webhook.Instance
	err := s.db.QueryRowContext(ctx, `
		SELECT id, base_url, status, webhook_secret_ref
		FROM gitlab_instances WHERE id = $1`, instanceID).
		Scan(&instance.ID, &instance.BaseURL, &instance.Status, &instance.WebhookSecretRef)
	if errors.Is(err, sql.ErrNoRows) {
		return webhook.Instance{}, false, nil
	}
	if err != nil {
		return webhook.Instance{}, false, fmt.Errorf("webhook store: instance by id: %w", err)
	}
	if instance.Status == "removed" {
		// Removed instances are hidden exactly like unknown ones: the
		// receiver surface must not resurrect retired hosts.
		return webhook.Instance{}, false, nil
	}
	return instance, true, nil
}

func (s pgWebhookStore) MappingWebhookUUID(ctx context.Context, instanceID string, gitlabProjectID int64) (string, bool, error) {
	var registered sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT webhook_uuid FROM gitlab_project_mappings
		WHERE gitlab_instance_id = $1 AND gitlab_project_id = $2`,
		instanceID, gitlabProjectID).Scan(&registered)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("webhook store: mapping webhook uuid: %w", err)
	}
	return registered.String, true, nil
}

const webhookDeliveryInsert = `
	INSERT INTO webhook_deliveries
		(inbox_id, gitlab_instance_id, external_event_id, event_kind, token_verified, outcome, reject_reason)
	VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, NULLIF($7, ''))`

func (s pgWebhookStore) RecordDenial(ctx context.Context, audit webhook.AuditRow) error {
	if _, err := s.db.ExecContext(ctx, webhookDeliveryInsert,
		audit.InboxID, audit.InstanceID, audit.ExternalEventID,
		audit.EventKind, audit.TokenVerified, audit.Outcome, audit.RejectReason); err != nil {
		return fmt.Errorf("webhook store: record denial: %w", err)
	}
	return nil
}

func (s pgWebhookStore) IngestDelivery(ctx context.Context, rec webhook.IngestRecord) (webhook.IngestResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return webhook.IngestResult{}, fmt.Errorf("webhook store: ingest begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	status := webhook.StatusReceived
	outcome := webhook.OutcomeAccepted
	reason := ""
	if rec.Quarantine {
		status = webhook.StatusDeadLetter
		outcome = webhook.OutcomeDeadLetter
		reason = rec.RejectReason
	}

	inboxID := pgNewUUID()
	err = tx.QueryRowContext(ctx, `
		INSERT INTO webhook_inbox
			(id, gitlab_instance_id, external_event_id, event_kind, webhook_uuid, payload_digest, raw_body_encrypted, status)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, $8)
		ON CONFLICT (gitlab_instance_id, external_event_id) DO NOTHING
		RETURNING id`,
		inboxID, rec.InstanceID, rec.ExternalEventID, rec.EventKind, rec.WebhookUUID,
		rec.PayloadDigest, rec.RawBodyEncrypted, status).Scan(&inboxID)

	switch {
	case err == nil:
		// New row: the audit entry below records the durable accept (or
		// the quarantine) for this delivery.
	case errors.Is(err, sql.ErrNoRows):
		// Exact re-delivery under the same dedup key: the business effect
		// already happened exactly once; audit as duplicate only. The
		// existing row's state is deliberately untouched — a re-delivery
		// must never reset or regress inbox progress (GL-INV-004).
		if lookupErr := tx.QueryRowContext(ctx, `
			SELECT id FROM webhook_inbox
			WHERE gitlab_instance_id = $1 AND external_event_id = $2`,
			rec.InstanceID, rec.ExternalEventID).Scan(&inboxID); lookupErr != nil {
			return webhook.IngestResult{}, fmt.Errorf("webhook store: ingest duplicate lookup: %w", lookupErr)
		}
		outcome = webhook.OutcomeDuplicate
		reason = ""
	default:
		return webhook.IngestResult{}, fmt.Errorf("webhook store: ingest insert: %w", err)
	}

	if _, err := tx.ExecContext(ctx, webhookDeliveryInsert,
		inboxID, rec.InstanceID, rec.ExternalEventID, rec.EventKind, true, outcome, reason); err != nil {
		return webhook.IngestResult{}, fmt.Errorf("webhook store: ingest audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return webhook.IngestResult{}, fmt.Errorf("webhook store: ingest commit: %w", err)
	}
	return webhook.IngestResult{Outcome: outcome, InboxID: inboxID}, nil
}

func (s pgWebhookStore) ClaimInbox(ctx context.Context, owner string) (*webhook.InboxRow, error) {
	if owner == "" {
		return nil, errors.New("webhook store: claim owner must not be empty")
	}
	row := &webhook.InboxRow{}
	var webhookUUID sql.NullString
	var sealed []byte
	var receivedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		UPDATE webhook_inbox SET
			status = 'processing', attempts = attempts + 1, lease_owner = $1, claimed_at = now()
		WHERE id = (
			SELECT id FROM webhook_inbox
			WHERE status = 'received'
			   OR (status = 'retry_wait' AND (next_attempt_at IS NULL OR next_attempt_at <= now()))
			   OR (status = 'processing' AND claimed_at < now() - $2::interval)
			ORDER BY received_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, gitlab_instance_id, external_event_id, event_kind, webhook_uuid,
			payload_digest, raw_body_encrypted, received_at, attempts`,
		owner, fmt.Sprintf("%.3f seconds", staleClaimAge.Seconds())).Scan(
		&row.ID, &row.InstanceID, &row.ExternalEventID, &row.EventKind, &webhookUUID,
		&row.PayloadDigest, &sealed, &receivedAt, &row.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("webhook store: claim inbox: %w", err)
	}
	row.WebhookUUID = webhookUUID.String
	row.RawBodyEncrypted = sealed
	row.ReceivedAt = pgTimeString(receivedAt)
	return row, nil
}

func (s pgWebhookStore) BeginApply(ctx context.Context) (webhook.ApplyUnit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("webhook store: begin apply: %w", err)
	}
	return &pgWebhookApply{tx: tx}, nil
}

func (s pgWebhookStore) ReplayDeadLetter(ctx context.Context, inboxID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE webhook_inbox
		SET status = 'received', next_attempt_at = NULL, lease_owner = NULL, claimed_at = NULL
		WHERE id = $1 AND status = 'dead_letter'`, inboxID)
	if err != nil {
		return false, fmt.Errorf("webhook store: replay dead letter: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

// pgWebhookApply settles one claimed row transactionally.
type pgWebhookApply struct{ tx *sql.Tx }

func (a *pgWebhookApply) ProjectForMapping(ctx context.Context, instanceID string, gitlabProjectID int64) (string, error) {
	var projectID string
	err := a.tx.QueryRowContext(ctx, `
		SELECT project_id::text FROM gitlab_project_mappings
		WHERE gitlab_instance_id = $1 AND gitlab_project_id = $2`,
		instanceID, gitlabProjectID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("webhook apply: project for mapping: %w", err)
	}
	return projectID, nil
}

func (a *pgWebhookApply) EnqueueOutbox(ctx context.Context, event *model.OutboxEvent) error {
	if err := (pgOutboxStore{q: a.tx}).Enqueue(ctx, event); err != nil {
		if errors.Is(err, ErrDuplicateEvent) {
			return webhook.ErrEnvelopeDuplicate
		}
		return err
	}
	return nil
}

func (a *pgWebhookApply) MarkProcessed(ctx context.Context, inboxID, owner string) error {
	return a.settle(ctx, `
		UPDATE webhook_inbox SET status = 'processed', processed_at = now(), lease_owner = NULL
		WHERE id = $1 AND lease_owner = $2`, inboxID, owner, "")
}

func (a *pgWebhookApply) MarkRetry(ctx context.Context, inboxID, owner string, availableIn time.Duration) error {
	return a.settle(ctx, `
		UPDATE webhook_inbox
		SET status = 'retry_wait', next_attempt_at = now() + $3::interval, lease_owner = NULL
		WHERE id = $1 AND lease_owner = $2`, inboxID, owner, "", fmt.Sprintf("%.3f seconds", availableIn.Seconds()))
}

func (a *pgWebhookApply) MarkDeadLetter(ctx context.Context, inboxID, owner, reason string) error {
	return a.settle(ctx, `
		UPDATE webhook_inbox SET status = 'dead_letter', lease_owner = NULL
		WHERE id = $1 AND lease_owner = $2`, inboxID, owner, reason)
}

// settle applies one settlement guarded by the dispatcher lease. A
// non-empty deadLetterReason additionally appends the DLQ audit entry in
// the same transaction.
func (a *pgWebhookApply) settle(ctx context.Context, statement, inboxID, owner, deadLetterReason string, extra ...any) error {
	args := append([]any{inboxID, owner}, extra...)
	result, err := a.tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("webhook apply: settle: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrWebhookClaimMismatch
	}
	if deadLetterReason == "" {
		return nil
	}
	var instanceID, externalID, eventKind string
	if err := a.tx.QueryRowContext(ctx, `
		SELECT gitlab_instance_id::text, external_event_id, event_kind
		FROM webhook_inbox WHERE id = $1`, inboxID).
		Scan(&instanceID, &externalID, &eventKind); err != nil {
		return fmt.Errorf("webhook apply: dead-letter audit lookup: %w", err)
	}
	if _, err := a.tx.ExecContext(ctx, webhookDeliveryInsert,
		inboxID, instanceID, externalID, eventKind, true, webhook.OutcomeDeadLetter, deadLetterReason); err != nil {
		return fmt.Errorf("webhook apply: dead-letter audit: %w", err)
	}
	return nil
}

func (a *pgWebhookApply) Commit() error {
	if err := a.tx.Commit(); err != nil {
		return fmt.Errorf("webhook apply: commit: %w", err)
	}
	return nil
}

func (a *pgWebhookApply) Rollback() error { return a.tx.Rollback() }
