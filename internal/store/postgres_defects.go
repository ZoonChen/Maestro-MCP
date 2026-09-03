package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/defect"
)

// PostgreSQL persistence for the DEF/DSP flow (M3-DEF-001/DSP-001):
// findings land with ingest idempotency, defects upsert on the
// versioned fingerprint, and duplicates only grow occurrence history
// (DEF-INV-002). Reopen semantics: a resolved defect that sees the
// same fingerprint again goes back to assigned.

// Sentinels for the upsert outcomes.
var (
	ErrFindingDuplicate = errors.New("finding already recorded for this source event")
	ErrDefectNotFound   = errors.New("defect not found in this project")
)

type pgDefectStore struct{ db *sql.DB }

// Defects returns the finding/defect store.
func (s *PostgresStore) Defects() pgDefectStore { return pgDefectStore{db: s.DB()} }

// RecordFinding persists one normalized finding and upserts its defect
// in ONE transaction: the defect's occurrence counter and last_seen
// grow atomically, a first sighting creates the defect at the finding's
// severity, a resolved defect reopens to assigned, and the occurrence
// row links defect to finding (DEF-INV-002). Returns the defect id and
// whether this delivery CREATED the defect.
func (s pgDefectStore) RecordFinding(ctx context.Context, projectID string, finding defect.Finding, fingerprint string) (defectID string, created bool, err error) {
	if fingerprint == "" {
		return "", false, errors.New("defects: fingerprint is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("defects: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingDefect sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id::text FROM defects
		WHERE project_id = $1 AND fingerprint_version = $2 AND fingerprint_hash = $3`,
		projectID, defect.FingerprintAlgorithm, fingerprint).Scan(&existingDefect)

	switch {
	case err == nil:
		defectID = existingDefect.String
		// Occurrence wins over resolved: the same fingerprint came back
		// (row-locked read-modify-write; severity only ever rises).
		var currentSeverity, currentState string
		if qErr := tx.QueryRowContext(ctx,
			`SELECT severity, state FROM defects WHERE id = $1 FOR UPDATE`,
			defectID).Scan(&currentSeverity, &currentState); qErr != nil {
			return "", false, fmt.Errorf("defects: lock: %w", qErr)
		}
		newSeverity := currentSeverity
		if defect.SeverityRank(finding.Severity) > defect.SeverityRank(defect.Severity(currentSeverity)) {
			newSeverity = string(finding.Severity)
		}
		newState := currentState
		if currentState == "resolved" {
			newState = "assigned"
		}
		if _, uErr := tx.ExecContext(ctx, `
			UPDATE defects SET occurrence = occurrence + 1, last_seen_at = now(),
				severity = $2, state = $3, version = version + 1, updated_at = now()
			WHERE id = $1`, defectID, newSeverity, newState); uErr != nil {
			return "", false, fmt.Errorf("defects: occurrence bump: %w", uErr)
		}
		created = false
	case errors.Is(err, sql.ErrNoRows):
		defectID = pgNewUUID()
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO defects (id, project_id, fingerprint_version, fingerprint_hash,
				state, severity, title, occurrence)
			VALUES ($1, $2, $3, $4, 'detected', $5, $6, 1)`,
			defectID, projectID, defect.FingerprintAlgorithm, fingerprint,
			finding.Severity, defectTitle(finding)); err != nil {
			return "", false, fmt.Errorf("defects: create: %w", err)
		}
		created = true
	default:
		return "", false, fmt.Errorf("defects: lookup: %w", err)
	}

	// The finding lands under its ingest identity; a replay is the
	// idempotency the (project, source_type, source_event_id) unique
	// key enforces — surfaced as the duplicate sentinel.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO findings (id, project_id, source_type, source_event_id, severity,
			environment, repro, evidence_refs, task_refs, adapter_version, payload_digest)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, '[]'::jsonb, $9, $10)
		ON CONFLICT (project_id, source_type, source_event_id) DO NOTHING`,
		pgNewUUID(), projectID, string(finding.SourceType), finding.SourceEventID,
		finding.Severity, finding.Environment, finding.Repro,
		evidenceJSON(finding.EvidenceRefs), finding.AdapterVersion,
		"sha256:"+digestOf(finding.SourceEventID+"\x00"+finding.Repro))
	if err != nil {
		return "", false, fmt.Errorf("defects: finding insert: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return "", false, ErrFindingDuplicate
	}

	// The occurrence link (defect, finding) is unique — replays cannot
	// double-count what the finding conflict already stopped.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO defect_occurrences (id, project_id, defect_id, finding_id)
		VALUES ($1, $2, $3, (SELECT id FROM findings WHERE project_id = $2
			AND source_type = $4 AND source_event_id = $5))
		ON CONFLICT (defect_id, finding_id) DO NOTHING`,
		pgNewUUID(), projectID, defectID, string(finding.SourceType), finding.SourceEventID); err != nil {
		return "", false, fmt.Errorf("defects: occurrence link: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", false, fmt.Errorf("defects: commit: %w", err)
	}
	return defectID, created, nil
}

// Defect resolves one defect by id within its project.
func (s pgDefectStore) Defect(ctx context.Context, projectID, defectID string) (state string, severity string, occurrence int, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT state, severity, occurrence FROM defects
		WHERE project_id = $1 AND id = $2`, projectID, defectID).
		Scan(&state, &severity, &occurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", 0, false, nil
	}
	if err != nil {
		return "", "", 0, false, fmt.Errorf("defects: get: %w", err)
	}
	return state, severity, occurrence, true, nil
}

func defectTitle(finding defect.Finding) string {
	title := finding.SourceEventID
	if len(title) > 200 {
		title = title[:200]
	}
	return title
}

func evidenceJSON(refs []string) string {
	if len(refs) == 0 {
		return "[]"
	}
	out := "["
	for index, ref := range refs {
		if index > 0 {
			out += ","
		}
		out += `"` + ref + `"`
	}
	return out + "]"
}

// digestOf is the finding payload digest (identity+repro bytes).
func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
