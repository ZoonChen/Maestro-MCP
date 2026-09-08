package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// PostgreSQL persistence for M4-REL-001: the backup/WAL run ledger.
// The lifecycle is running -> completed -> verified | failed; guarded
// UPDATEs carry the state machine and the migration 0013 CHECK
// constraints are the structural backstop for REL-RULE-004 — a run is
// 'verified' only with an off-host restore, a matched checksum and a
// passed business smoke. RPO/freshness readers (the frozen SLO
// objectives rpo_minutes / backup_success_rate_percent) consume
// LatestVerifiedBackup and CountBackupRuns.

// Backup sentinels.
var (
	ErrBackupRunRejected   = errors.New("backup run rejected")
	ErrBackupRunNotFound   = errors.New("backup run not found")
	ErrBackupStateConflict = errors.New("backup run state changed concurrently")
)

var (
	backupDigestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	backupStorageRefRegex = regexp.MustCompile(`^env:MAESTRO_[A-Z0-9_]+$`)
)

type pgBackupStore struct{ db *sql.DB }

// Reliability returns the backup/recovery store.
func (s *PostgresStore) Reliability() pgBackupStore {
	return pgBackupStore{db: s.DB()}
}

// BackupRun is one backup run row as read back.
type BackupRun struct {
	ID                     string
	Kind                   string
	Status                 string
	StartedAt              time.Time
	FinishedAt             *time.Time
	SizeBytes              *int64
	Digest                 string
	StorageRef             string
	RestoreVerifiedAt      *time.Time
	RestoreVerifiedHost    string
	RestoreChecksumMatched bool
	RestoreSmokePassed     bool
}

// RecordBackupStart opens one run in the running state.
func (s pgBackupStore) RecordBackupStart(ctx context.Context, kind string, startedAt time.Time) (string, error) {
	if kind != "full" && kind != "wal" {
		return "", fmt.Errorf("%w: kind must be full or wal, got %q", ErrBackupRunRejected, kind)
	}
	id := pgNewUUID()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO backup_runs (id, kind, status, started_at)
		VALUES ($1, $2, 'running', $3)`,
		id, kind, startedAt); err != nil {
		return "", fmt.Errorf("backup: start: %w", err)
	}
	return id, nil
}

// CompleteBackup records the artifact facts on a running run. The
// digest must be the sha256:... form and the storage ref an
// env:MAESTRO_* secret reference — artifact locations are never
// inline credentials.
func (s pgBackupStore) CompleteBackup(ctx context.Context, id string, finishedAt time.Time, sizeBytes int64, digest, storageRef string) error {
	if sizeBytes <= 0 || !backupDigestPattern.MatchString(digest) || !backupStorageRefRegex.MatchString(storageRef) {
		return fmt.Errorf("%w: size, sha256 digest and env: storage ref are required", ErrBackupRunRejected)
	}
	return s.guardedUpdate(ctx, id, `
		UPDATE backup_runs
		SET status = 'completed', finished_at = $2, size_bytes = $3, sha256_digest = $4, storage_ref = $5
		WHERE id = $1 AND status = 'running'`,
		finishedAt, sizeBytes, digest, storageRef)
}

// RecordRestoreVerification closes a completed run per REL-RULE-004:
// both the checksum match and the business smoke must hold for
// 'verified', anything else lands 'failed' with the evidence kept.
func (s pgBackupStore) RecordRestoreVerification(ctx context.Context, id string, verifiedAt time.Time, verifiedHost string, checksumMatched, smokePassed bool) error {
	if verifiedHost == "" {
		return fmt.Errorf("%w: the restoring host is required — same-host self-attestation is not restore evidence", ErrBackupRunRejected)
	}
	status := "failed"
	if checksumMatched && smokePassed {
		status = "verified"
	}
	return s.guardedUpdate(ctx, id, `
		UPDATE backup_runs
		SET status = $2, restore_verified_at = $3, restore_verified_host = $4,
			restore_checksum_matched = $5, restore_smoke_passed = $6
		WHERE id = $1 AND status = 'completed'`,
		status, verifiedAt, verifiedHost, checksumMatched, smokePassed)
}

// MarkBackupFailed terminally fails a run that is still running or
// completed (an aborted backup attempt or an unverifiable artifact).
func (s pgBackupStore) MarkBackupFailed(ctx context.Context, id string, failedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE backup_runs SET status = 'failed', finished_at = $2
		WHERE id = $1 AND status IN ('running', 'completed')`, id, failedAt)
	if err != nil {
		return fmt.Errorf("backup: fail: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return s.absentOrConflict(ctx, id)
	}
	return nil
}

// LatestVerifiedBackup returns the newest verified run of one kind
// started at or before notAfter — the RPO anchor. No verified backup
// means no data, never a zero age.
func (s pgBackupStore) LatestVerifiedBackup(ctx context.Context, kind string, notAfter time.Time) (*BackupRun, error) {
	row := s.db.QueryRowContext(ctx, backupRunSelect+`
		WHERE kind = $1 AND status = 'verified' AND started_at <= $2
		ORDER BY started_at DESC LIMIT 1`, kind, notAfter)
	run, err := scanBackupRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("backup: latest verified: %w", err)
	}
	return run, nil
}

// CountBackupRuns counts runs of one kind and status started inside
// the half-open interval [from, to) — the backup-success-rate input.
func (s pgBackupStore) CountBackupRuns(ctx context.Context, kind, status string, from, to time.Time) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM backup_runs
		WHERE kind = $1 AND status = $2 AND started_at >= $3 AND started_at < $4`,
		kind, status, from, to).Scan(&count); err != nil {
		return 0, fmt.Errorf("backup: count: %w", err)
	}
	return count, nil
}

const backupRunSelect = `
	SELECT id::text, kind, status, started_at, finished_at, size_bytes,
		COALESCE(sha256_digest, ''), COALESCE(storage_ref, ''),
		restore_verified_at, COALESCE(restore_verified_host, ''),
		restore_checksum_matched, restore_smoke_passed
	FROM backup_runs`

type backupRowScanner interface{ Scan(dest ...any) error }

func scanBackupRun(row backupRowScanner) (*BackupRun, error) {
	run := BackupRun{}
	err := row.Scan(&run.ID, &run.Kind, &run.Status, &run.StartedAt, &run.FinishedAt,
		&run.SizeBytes, &run.Digest, &run.StorageRef,
		&run.RestoreVerifiedAt, &run.RestoreVerifiedHost,
		&run.RestoreChecksumMatched, &run.RestoreSmokePassed)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// guardedUpdate runs one state-machine-guarded UPDATE (the guard
// lives in the WHERE clause) and maps zero affected rows to
// not-found versus state-conflict.
func (s pgBackupStore) guardedUpdate(ctx context.Context, id, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, append([]any{id}, args...)...)
	if err != nil {
		return fmt.Errorf("backup: update: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return s.absentOrConflict(ctx, id)
	}
	return nil
}

func (s pgBackupStore) absentOrConflict(ctx context.Context, id string) error {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM backup_runs WHERE id = $1)`, id).Scan(&exists); err != nil {
		return fmt.Errorf("backup: exists: %w", err)
	}
	if !exists {
		return ErrBackupRunNotFound
	}
	return ErrBackupStateConflict
}
