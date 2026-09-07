package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
)

// PostgreSQL persistence for M4-EVAL-001: the four-layer evaluation
// records over the migration 0012 table (plus 0014's
// trajectory_constraints column). Records are append-only and unique
// per (run, case, layer) — the harness's repeated trials carry
// distinct case ids; the wire validation lives in internal/eval and
// the table CHECKs are the structural backstop.

// Evaluation sentinels.
var (
	ErrEvaluationDuplicate = errors.New("evaluation record already exists for this run, case and layer")
)

type pgEvaluationStore struct{ db *sql.DB }

// Evaluation returns the evaluation-record store.
func (s *PostgresStore) Evaluation() pgEvaluationStore {
	return pgEvaluationStore{db: s.DB()}
}

// AppendRecord validates one record against the frozen wire contract
// and appends it; the stored id is returned (generated when unset).
// A conflicting (run, case, layer) key is a duplicate, not an update.
func (s pgEvaluationStore) AppendRecord(ctx context.Context, record eval.Record) (string, error) {
	if err := record.Validate(); err != nil {
		return "", err
	}
	trajectory, err := marshalStrings(record.TrajectoryConstraints)
	if err != nil {
		return "", err
	}
	forbidden, err := marshalStrings(record.ForbiddenActionsObserved)
	if err != nil {
		return "", err
	}
	evidence, err := marshalStrings(record.EvidenceRefs)
	if err != nil {
		return "", err
	}
	id := record.RecordID
	if id == "" {
		id = pgNewUUID()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO evaluation_records
			(id, project_id, layer, case_id, run_id, category, risk, verdict, score,
			 scorer_kind, scorer_version, calibration_ref, dataset_digest,
			 model_config_digest, seed, trajectory_constraints,
			 forbidden_actions_observed, tokens_used, latency_ms, external_state_digest,
			 evidence_refs, notes, version)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, '')::text, $8, $9,
			$10, $11, NULLIF($12, ''), $13,
			NULLIF($14, ''), $15, $16, $17, $18, $19, NULLIF($20, ''),
			$21, NULLIF($22, ''), $23)
		ON CONFLICT (run_id, case_id, layer) DO NOTHING`,
		id, nilIfEmpty(record.ProjectID), string(record.Layer), record.CaseID, record.RunID,
		record.Category, string(record.Risk), string(record.Verdict), record.Score,
		string(record.Scorer.Kind), record.Scorer.Version, record.Scorer.CalibrationRef,
		record.DatasetDigest, record.ModelConfigDigest, record.Seed, trajectory,
		forbidden, record.TokensUsed, record.LatencyMS, record.ExternalStateDigest,
		evidence, record.Notes, record.Version)
	if err != nil {
		return "", fmt.Errorf("evaluation: append: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return "", ErrEvaluationDuplicate
	}
	return id, nil
}

// RecordsByRun reads one run's records for a layer in case order.
func (s pgEvaluationStore) RecordsByRun(ctx context.Context, runID string, layer eval.Layer) ([]eval.Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, COALESCE(project_id::text, ''), layer, case_id, run_id,
			COALESCE(category, ''), COALESCE(risk, ''), verdict, score,
			scorer_kind, scorer_version, COALESCE(calibration_ref, ''),
			dataset_digest, COALESCE(model_config_digest, ''), seed,
			trajectory_constraints, forbidden_actions_observed,
			tokens_used, latency_ms, COALESCE(external_state_digest, ''),
			evidence_refs, COALESCE(notes, ''), version
		FROM evaluation_records
		WHERE run_id = $1 AND layer = $2
		ORDER BY case_id`, runID, string(layer))
	if err != nil {
		return nil, fmt.Errorf("evaluation: by run: %w", err)
	}
	defer rows.Close()

	records := []eval.Record{}
	for rows.Next() {
		record := eval.Record{}
		var trajectory, forbidden, evidence []byte
		if scanErr := rows.Scan(&record.RecordID, &record.ProjectID, &record.Layer,
			&record.CaseID, &record.RunID, &record.Category, &record.Risk,
			&record.Verdict, &record.Score, &record.Scorer.Kind, &record.Scorer.Version,
			&record.Scorer.CalibrationRef, &record.DatasetDigest, &record.ModelConfigDigest,
			&record.Seed, &trajectory, &forbidden, &record.TokensUsed, &record.LatencyMS,
			&record.ExternalStateDigest, &evidence, &record.Notes, &record.Version); scanErr != nil {
			return nil, fmt.Errorf("evaluation: scan: %w", scanErr)
		}
		if record.TrajectoryConstraints, err = unmarshalStrings(trajectory); err != nil {
			return nil, err
		}
		if record.ForbiddenActionsObserved, err = unmarshalStrings(forbidden); err != nil {
			return nil, err
		}
		if record.EvidenceRefs, err = unmarshalStrings(evidence); err != nil {
			return nil, err
		}
		record.SchemaVersion = eval.SchemaVersion
		records = append(records, record)
	}
	return records, rows.Err()
}

// VerdictCounts aggregates one run's verdicts across layers — the
// pass-rate/pass^k input alongside the trial records.
func (s pgEvaluationStore) VerdictCounts(ctx context.Context, runID string) (map[eval.Verdict]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT verdict, count(*) FROM evaluation_records
		WHERE run_id = $1 GROUP BY verdict`, runID)
	if err != nil {
		return nil, fmt.Errorf("evaluation: verdict counts: %w", err)
	}
	defer rows.Close()

	counts := map[eval.Verdict]int64{}
	for rows.Next() {
		var verdict eval.Verdict
		var count int64
		if scanErr := rows.Scan(&verdict, &count); scanErr != nil {
			return nil, fmt.Errorf("evaluation: verdict scan: %w", scanErr)
		}
		counts[verdict] = count
	}
	return counts, rows.Err()
}

func marshalStrings(values []string) ([]byte, error) {
	if len(values) == 0 {
		return []byte("[]"), nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("evaluation: encode: %w", err)
	}
	return encoded, nil
}

func unmarshalStrings(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("evaluation: decode: %w", err)
	}
	return values, nil
}

func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
