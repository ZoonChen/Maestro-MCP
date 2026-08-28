package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Shared helpers for the PostgreSQL store layer. The frozen model entities
// carry RFC3339 string timestamps (M0 wire style); these helpers convert at
// the driver boundary instead of growing a parallel struct family.

// pgArg passes a string UUID parameter straight through: pgx encodes
// string <-> uuid natively, and empty strings surface as invalid input at
// the database boundary instead of being coerced.
func pgArg(value string) string { return value }

// pgNewUUID mints a UUIDv7 identifier for rows the store creates itself.
func pgNewUUID() string { return newImportUUID().String() }

// pgStr normalizes scanned UUID text (pgx already returns canonical form).
func pgStr(value string) string { return value }

// pgTimeString renders a database timestamp as RFC3339 UTC text.
func pgTimeString(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// pgTimeArg parses an RFC3339-ish timestamp for a timestamptz parameter.
// Unparseable input is passed through so PostgreSQL reports the bad value
// instead of the store silently dropping it.
func pgTimeArg(value string) any {
	if parsed, err := parseSQLiteTimestamp(value); err == nil {
		return parsed
	}
	return value
}

// pgOptionalTime maps an empty string to SQL NULL.
func pgOptionalTime(value string) any {
	if value == "" {
		return nil
	}
	return pgTimeArg(value)
}

// pgOptionalTimePtr maps a nil/empty string pointer to SQL NULL.
func pgOptionalTimePtr(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return pgTimeArg(*value)
}

// pgOptionalJSON maps absent payload bytes to SQL NULL.
func pgOptionalJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

const pgEnvelopeColumns = `event_id, event_type, event_version, source, project_id, subject,
	occurred_at, correlation_id, causation_id, payload_digest, sensitivity, payload`

// scanPGEnvelope reads the twelve envelope columns; extra may append
// table-specific bookkeeping destinations to the same scan call.
func scanPGEnvelope(scan func(dest ...any) error, envelope *model.EventEnvelope, extra func(dest *[]any)) error {
	var occurredAt time.Time
	var payload []byte
	dest := []any{
		&envelope.EventID, &envelope.EventType, &envelope.EventVersion, &envelope.Source,
		&envelope.ProjectID, &envelope.Subject, &occurredAt, &envelope.CorrelationID,
		&envelope.CausationID, &envelope.PayloadDigest, &envelope.Sensitivity, &payload,
	}
	if extra != nil {
		extra(&dest)
	}
	if err := scan(dest...); err != nil {
		return err
	}
	envelope.OccurredAt = pgTimeString(occurredAt)
	envelope.Payload = payload
	return nil
}

// ---------------------------------------------------------------------------
// Outbox (ADR-002)
// ---------------------------------------------------------------------------

type pgOutboxStore struct{ q pgExecer }

func (s pgOutboxStore) Enqueue(ctx context.Context, e *model.OutboxEvent) error {
	if e.EventID == "" {
		e.EventID = pgNewUUID()
	}
	status := e.Status
	if status == "" {
		status = "pending"
	}
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO outbox_events (`+pgEnvelopeColumns+`, status, available_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13, COALESCE($14::timestamptz, now()))
		ON CONFLICT (event_id) DO NOTHING`,
		e.EventID, e.EventType, e.EventVersion, e.Source, e.ProjectID, e.Subject,
		pgTimeArg(e.OccurredAt), e.CorrelationID, e.CausationID, e.PayloadDigest,
		e.Sensitivity, pgOptionalJSON(e.Payload), status, pgOptionalTime(e.AvailableAt))
	if err != nil {
		return fmt.Errorf("outbox: enqueue: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

// ClaimPending leases up to batchSize dispatchable events to exactly one
// owner using FOR UPDATE SKIP LOCKED; competing dispatchers never overlap.
// The frozen contract carries a caller-supplied now for driver-portability;
// this implementation deliberately uses the database clock so claims and
// availability windows share one time domain.
func (s pgOutboxStore) ClaimPending(ctx context.Context, batchSize int, owner, _ string) ([]*model.OutboxEvent, error) {
	if batchSize < 1 {
		return nil, errors.New("outbox: batch size must be positive")
	}
	if owner == "" {
		return nil, errors.New("outbox: claim owner must not be empty")
	}
	rows, err := s.q.QueryContext(ctx, `
		UPDATE outbox_events o SET
			status = 'sending',
			attempts = o.attempts + 1,
			lease_owner = $1,
			updated_at = now()
		WHERE o.event_id IN (
			SELECT event_id FROM outbox_events
			WHERE status IN ('pending', 'retry_wait') AND available_at <= now()
			ORDER BY available_at, event_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING o.event_id, o.event_type, o.event_version, o.source, o.project_id, o.subject,
			o.occurred_at, o.correlation_id, o.causation_id, o.payload_digest, o.sensitivity, o.payload,
			o.status, o.attempts, o.available_at, o.lease_owner, o.created_at, o.updated_at`,
		owner, batchSize)
	if err != nil {
		return nil, fmt.Errorf("outbox: claim pending: %w", err)
	}
	defer rows.Close()
	events := []*model.OutboxEvent{}
	for rows.Next() {
		event := &model.OutboxEvent{}
		var leaseOwner sql.NullString
		var availableAt, createdAt, updatedAt time.Time
		err := scanPGEnvelope(rows.Scan, &event.EventEnvelope, func(dest *[]any) {
			*dest = append(*dest, &event.Status, &event.Attempts, &availableAt, &leaseOwner, &createdAt, &updatedAt)
		})
		if err != nil {
			return nil, fmt.Errorf("outbox: scan claimed event: %w", err)
		}
		event.AvailableAt = pgTimeString(availableAt)
		event.CreatedAt = pgTimeString(createdAt)
		event.UpdatedAt = pgTimeString(updatedAt)
		if leaseOwner.Valid {
			event.LeaseOwner = &leaseOwner.String
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s pgOutboxStore) MarkDelivered(ctx context.Context, eventID, owner string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_events SET status = 'delivered', lease_owner = NULL, updated_at = now()
		WHERE event_id = $1 AND lease_owner = $2`,
		eventID, owner)
	if err != nil {
		return fmt.Errorf("outbox: mark delivered: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return s.outboxClaimMismatch(ctx, eventID)
	}
	return nil
}

func (s pgOutboxStore) MarkRetry(ctx context.Context, eventID, owner string, attempts int, availableAt string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_events
		SET status = 'retry_wait', attempts = GREATEST(attempts, $3), available_at = $4::timestamptz,
		    lease_owner = NULL, updated_at = now()
		WHERE event_id = $1 AND lease_owner = $2`,
		eventID, owner, attempts, pgTimeArg(availableAt))
	if err != nil {
		return fmt.Errorf("outbox: mark retry: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return s.outboxClaimMismatch(ctx, eventID)
	}
	return nil
}

func (s pgOutboxStore) MarkDeadLetter(ctx context.Context, eventID, owner string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE outbox_events SET status = 'dead_letter', lease_owner = NULL, updated_at = now()
		WHERE event_id = $1 AND lease_owner = $2`,
		eventID, owner)
	if err != nil {
		return fmt.Errorf("outbox: mark dead letter: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return s.outboxClaimMismatch(ctx, eventID)
	}
	return nil
}

func (s pgOutboxStore) outboxClaimMismatch(ctx context.Context, eventID string) error {
	var exists int
	err := s.q.QueryRowContext(ctx,
		`SELECT 1 FROM outbox_events WHERE event_id = $1`, eventID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDuplicateEvent
	}
	if err != nil {
		return fmt.Errorf("outbox: verify claim: %w", err)
	}
	return ErrOutboxClaimMismatch
}

// ---------------------------------------------------------------------------
// Inbox (ADR-002)
// ---------------------------------------------------------------------------

type pgInboxStore struct{ q pgExecer }

// Record inserts a received event exactly once: a duplicate event identity
// reports created=false so consumers stay idempotent.
func (s pgInboxStore) Record(ctx context.Context, e *model.InboxEvent) (bool, error) {
	if e.EventID == "" {
		return false, errors.New("inbox: event_id must not be empty")
	}
	status := e.Status
	if status == "" {
		status = "received"
	}
	result, err := s.q.ExecContext(ctx, `
		INSERT INTO inbox_events (`+pgEnvelopeColumns+`, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13)
		ON CONFLICT (event_id) DO NOTHING`,
		e.EventID, e.EventType, e.EventVersion, e.Source, e.ProjectID, e.Subject,
		pgTimeArg(e.OccurredAt), e.CorrelationID, e.CausationID, e.PayloadDigest,
		e.Sensitivity, pgOptionalJSON(e.Payload), status)
	if err != nil {
		return false, fmt.Errorf("inbox: record: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

func (s pgInboxStore) ClaimProcessing(ctx context.Context, eventID string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'processing'
		WHERE event_id = $1 AND status = 'received'`,
		eventID)
	if err != nil {
		return fmt.Errorf("inbox: claim processing: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

func (s pgInboxStore) MarkProcessed(ctx context.Context, eventID string) error {
	result, err := s.q.ExecContext(ctx, `
		UPDATE inbox_events SET status = 'processed', processed_at = now()
		WHERE event_id = $1 AND status = 'processing'`,
		eventID)
	if err != nil {
		return fmt.Errorf("inbox: mark processed: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrDuplicateEvent
	}
	return nil
}
