package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PG-gated M4-REL-001 backup ledger: the running -> completed ->
// verified|failed lifecycle with REL-RULE-004 as a table invariant —
// verification without the off-host restore evidence is refused at
// the SQL layer, not by convention.

func TestBackupRunLifecycle(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_backup_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_backup_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_backup_test WITH (FORCE)`)
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_backup_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	rel := pg.Reliability()
	base := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)

	// Input rejection before any row exists.
	_, err = rel.RecordBackupStart(ctx, "incremental", base)
	require.ErrorIs(t, err, ErrBackupRunRejected)

	healthy, err := rel.RecordBackupStart(ctx, "full", base)
	require.NoError(t, err)

	err = rel.CompleteBackup(ctx, healthy, base.Add(10*time.Minute), 0,
		"sha256:"+strings.Repeat("a", 64), "env:MAESTRO_BACKUP_PRIMARY")
	require.ErrorIs(t, err, ErrBackupRunRejected, "zero-size artifacts are refused")
	err = rel.CompleteBackup(ctx, healthy, base.Add(10*time.Minute), 1<<30,
		"md5:abc", "env:MAESTRO_BACKUP_PRIMARY")
	require.ErrorIs(t, err, ErrBackupRunRejected, "only sha256: digests are accepted")
	err = rel.CompleteBackup(ctx, healthy, base.Add(10*time.Minute), 1<<30,
		"sha256:"+strings.Repeat("a", 64), "file:///etc/passwd")
	require.ErrorIs(t, err, ErrBackupRunRejected, "artifact refs must be env: secret references")

	// Completion on the running row; guards on repeat and on unknown ids.
	require.NoError(t, rel.CompleteBackup(ctx, healthy, base.Add(10*time.Minute), 1<<30,
		"sha256:"+strings.Repeat("a", 64), "env:MAESTRO_BACKUP_PRIMARY"))
	require.ErrorIs(t, rel.CompleteBackup(ctx, healthy, base.Add(12*time.Minute), 1<<30,
		"sha256:"+strings.Repeat("a", 64), "env:MAESTRO_BACKUP_PRIMARY"), ErrBackupStateConflict)
	require.ErrorIs(t, rel.CompleteBackup(ctx, "00000000-0000-7000-8000-0000000000ff",
		base.Add(time.Minute), 1, "sha256:"+strings.Repeat("b", 64), "env:MAESTRO_BACKUP_PRIMARY"),
		ErrBackupRunNotFound)

	// The structural backstop: flipping to verified without the
	// REL-RULE-004 evidence dies on the CHECK constraint.
	_, err = db.ExecContext(ctx, `UPDATE backup_runs SET status = 'verified' WHERE id = $1`, healthy)
	require.Error(t, err, "verified requires the restore evidence columns")

	// A checksum match without the business smoke is a FAILED backup,
	// never a verified one — and the evidence is kept.
	err = rel.RecordRestoreVerification(ctx, healthy, base.Add(30*time.Minute), "", true, true)
	require.ErrorIs(t, err, ErrBackupRunRejected, "same-host self-attestation is not restore evidence")
	require.NoError(t, rel.RecordRestoreVerification(ctx, healthy, base.Add(30*time.Minute),
		"restore-host-2.internal", true, false))
	run, err := rel.LatestVerifiedBackup(ctx, "full", base.Add(time.Hour))
	require.NoError(t, err)
	assert.Nil(t, run, "the failed run is not an RPO anchor")

	// The honest lifecycle: start, complete, verify off-host.
	good, err := rel.RecordBackupStart(ctx, "full", base.Add(24*time.Hour))
	require.NoError(t, err)
	require.NoError(t, rel.CompleteBackup(ctx, good, base.Add(24*time.Hour+time.Minute), 1<<31,
		"sha256:"+strings.Repeat("c", 64), "env:MAESTRO_BACKUP_SECONDARY"))
	require.NoError(t, rel.RecordRestoreVerification(ctx, good, base.Add(26*time.Hour),
		"restore-host-2.internal", true, true))

	run, err = rel.LatestVerifiedBackup(ctx, "full", base.Add(48*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "verified", run.Status)
	assert.Equal(t, "restore-host-2.internal", run.RestoreVerifiedHost)
	assert.True(t, run.RestoreChecksumMatched && run.RestoreSmokePassed)
	assert.True(t, run.StartedAt.Equal(base.Add(24*time.Hour)))
	require.NotNil(t, run.FinishedAt)
	require.NotNil(t, run.SizeBytes)
	assert.Equal(t, int64(1<<31), *run.SizeBytes)
	assert.Equal(t, "env:MAESTRO_BACKUP_SECONDARY", run.StorageRef)

	// RPO anchor semantics: before the start there is nothing.
	run, err = rel.LatestVerifiedBackup(ctx, "full", base)
	require.NoError(t, err)
	assert.Nil(t, run)

	// A WAL run and the failure path from running.
	wal, err := rel.RecordBackupStart(ctx, "wal", base.Add(25*time.Hour))
	require.NoError(t, err)
	require.NoError(t, rel.MarkBackupFailed(ctx, wal, base.Add(25*time.Hour+5*time.Minute)))
	require.ErrorIs(t, rel.CompleteBackup(ctx, wal, base.Add(26*time.Hour), 1<<20,
		"sha256:"+strings.Repeat("d", 64), "env:MAESTRO_BACKUP_PRIMARY"),
		ErrBackupStateConflict, "terminal states are one-way")

	// Counts feed the backup-success-rate objective; the window is
	// half-open on started_at.
	verified, err := rel.CountBackupRuns(ctx, "full", "verified", base, base.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), verified)
	failed, err := rel.CountBackupRuns(ctx, "full", "failed", base, base.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), failed)
	lateOnly, err := rel.CountBackupRuns(ctx, "full", "verified", base.Add(23*time.Hour), base.Add(48*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), lateOnly)
	earlyOnly, err := rel.CountBackupRuns(ctx, "full", "verified", base, base.Add(23*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(0), earlyOnly)
}
