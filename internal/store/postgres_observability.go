package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/audit"
)

// PostgreSQL persistence for M4-OBS-001: telemetry aggregate upserts
// and the audit-chain export over the immutable audit_events table.

// ObservabilitySentinels.
var (
	ErrTelemetryWindowRejected = errors.New("telemetry aggregate rejected")
	ErrAuditRangeEmpty         = errors.New("audit export range has no rows")
)

type pgObservabilityStore struct{ db *sql.DB }

// Observability returns the telemetry/audit store.
func (s *PostgresStore) Observability() pgObservabilityStore {
	return pgObservabilityStore{db: s.DB()}
}

// TelemetryPoint is one pre-aggregated, redacted data point from a
// producer that already applied the redaction policy.
type TelemetryPoint struct {
	ProjectID        string
	Metric           string
	WindowKind       string
	WindowStart      string
	SampleCount      int64
	Sum              float64
	Min, Max         *float64
	P50, P95, P99    *float64
	RedactionVersion string
}

// RecordTelemetry upserts one aggregate window bucket; the digest of
// the values participates in the conflict update so re-reported
// windows converge instead of silently diverging.
func (s pgObservabilityStore) RecordTelemetry(ctx context.Context, point TelemetryPoint) error {
	if point.SampleCount < 0 || point.Metric == "" || point.RedactionVersion == "" {
		return fmt.Errorf("%w: metric, sample count and redaction version are required", ErrTelemetryWindowRejected)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO telemetry_aggregates
			(id, project_id, metric, window_kind, window_start, sample_count,
			 sum_value, min_value, max_value, p50_value, p95_value, p99_value, redaction_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (project_id, metric, window_kind, window_start) DO UPDATE SET
			sample_count = EXCLUDED.sample_count,
			sum_value = EXCLUDED.sum_value,
			min_value = EXCLUDED.min_value,
			max_value = EXCLUDED.max_value,
			p50_value = EXCLUDED.p50_value,
			p95_value = EXCLUDED.p95_value,
			p99_value = EXCLUDED.p99_value,
			redaction_version = EXCLUDED.redaction_version`,
		pgNewUUID(), point.ProjectID, point.Metric, point.WindowKind,
		pgTimeArg(point.WindowStart), point.SampleCount, point.Sum,
		optFloat(point.Min), optFloat(point.Max), optFloat(point.P50),
		optFloat(point.P95), optFloat(point.P99), point.RedactionVersion); err != nil {
		return fmt.Errorf("observability: telemetry upsert: %w", err)
	}
	return nil
}

// AuditExport walks the immutable audit_events table in id (seq) order
// and returns the wire rows plus their per-entry digests and the
// rolling chain digest. The projection NEVER includes token hashes or
// free-text reasons — the exported fields are the redaction policy's
// allowlist per the frozen export schema.
func (s pgObservabilityStore) AuditExport(ctx context.Context, projectID string, fromSeq, toSeq int64) (rows []audit.Row, digests []string, chain string, err error) {
	if toSeq < fromSeq {
		return nil, nil, "", ErrAuditRangeEmpty
	}
	query := `
		SELECT id, correlation_id, action, actor_principal, created_at,
			COALESCE(resource_type || ':' || resource_id, ''), decision
		FROM audit_events
		WHERE project_id = $1 AND id BETWEEN $2 AND $3
		ORDER BY id`
	args := []any{projectID, fromSeq, toSeq}
	rowsIter, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, "", fmt.Errorf("observability: audit export: %w", err)
	}
	defer rowsIter.Close()

	for rowsIter.Next() {
		row := audit.Row{}
		var occurredAt []byte
		if scanErr := rowsIter.Scan(&row.Seq, &row.CorrelationID, &row.Action,
			&row.Principal, &occurredAt, &row.Resource, &row.Decision); scanErr != nil {
			return nil, nil, "", fmt.Errorf("observability: audit scan: %w", scanErr)
		}
		row.EventID = fmt.Sprintf("audit-%d", row.Seq)
		row.EventType = "audit.event"
		row.OccurredAt = string(occurredAt)
		rows = append(rows, row)
	}
	if iterErr := rowsIter.Err(); iterErr != nil {
		return nil, nil, "", fmt.Errorf("observability: audit iter: %w", iterErr)
	}
	digests, chain = audit.Chain(rows)
	return rows, digests, chain, nil
}

// AuditChainVerify recomputes the chain for a range and compares the
// stored events against the claimed digests — the tamper check an
// auditor runs on an export.
func (s pgObservabilityStore) AuditChainVerify(ctx context.Context, projectID string, fromSeq, toSeq int64, claimed []string) error {
	rows, _, _, err := s.AuditExport(ctx, projectID, fromSeq, toSeq)
	if err != nil {
		return err
	}
	if len(rows) != len(claimed) && len(claimed) != 0 {
		return fmt.Errorf("observability: audit verify: %d stored rows vs %d claimed", len(rows), len(claimed))
	}
	recomputed, _ := audit.Chain(rows)
	for index := range recomputed {
		if index >= len(claimed) {
			break
		}
		if recomputed[index] != claimed[index] {
			return fmt.Errorf("observability: audit verify: entry %d digest mismatch", index+1)
		}
	}
	return nil
}

func optFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
