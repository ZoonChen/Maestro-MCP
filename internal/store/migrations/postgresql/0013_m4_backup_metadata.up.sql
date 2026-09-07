-- M4 backup/WAL recovery metadata (P4, S1-led).
--
-- Authority: docs/delivery/m4-governance-console.md (M4-REL-001) and
-- operations/reliability-and-recovery.md (REL-REQ-002, REL-RULE-004).
-- The frozen slo-status.schema.json objectives rpo_minutes /
-- backup_success_rate_percent read these rows. REL-RULE-004 is a
-- table invariant here: a run reaches 'verified' only with an
-- off-host restore, a matched checksum and a passed business smoke.

CREATE TABLE backup_runs (
    id                       uuid PRIMARY KEY,
    kind                     text NOT NULL CHECK (kind IN ('full', 'wal')),
    status                   text NOT NULL DEFAULT 'running'
                             CHECK (status IN ('running', 'completed', 'verified', 'failed')),
    started_at               timestamptz NOT NULL,
    finished_at              timestamptz,
    size_bytes               bigint CHECK (size_bytes IS NULL OR size_bytes > 0),
    sha256_digest            text CHECK (sha256_digest IS NULL OR sha256_digest ~ '^sha256:[0-9a-f]{64}$'),
    storage_ref              text CHECK (storage_ref IS NULL OR storage_ref ~ '^env:MAESTRO_[A-Z0-9_]+$'),
    restore_verified_at      timestamptz,
    restore_verified_host    text,
    restore_checksum_matched boolean NOT NULL DEFAULT false,
    restore_smoke_passed     boolean NOT NULL DEFAULT false,
    created_at               timestamptz NOT NULL DEFAULT now(),
    -- Completion requires the artifact facts; verification requires the
    -- full REL-RULE-004 evidence. The lifecycle direction
    -- (running -> completed -> verified|failed) is carried by the
    -- guarded UPDATEs in the store; these CHECKs lock the per-status
    -- shape so no path can skip the evidence.
    CHECK (status <> 'completed' OR (finished_at IS NOT NULL AND finished_at >= started_at
        AND size_bytes IS NOT NULL AND sha256_digest IS NOT NULL AND storage_ref IS NOT NULL)),
    CHECK (status <> 'verified' OR (restore_verified_at IS NOT NULL
        AND restore_verified_at >= started_at
        AND restore_verified_host IS NOT NULL
        AND restore_checksum_matched AND restore_smoke_passed)),
    CHECK (status NOT IN ('failed', 'verified') OR finished_at IS NOT NULL),
    CHECK (restore_verified_at IS NULL OR status IN ('verified', 'failed'))
);
CREATE INDEX idx_backup_runs_freshness ON backup_runs (kind, status, started_at DESC);
