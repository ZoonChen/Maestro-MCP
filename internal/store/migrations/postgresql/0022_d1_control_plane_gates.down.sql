-- Best-effort down for D1: reverting the authority enum fails while
-- control_plane rows exist; delete them first (the judgments re-mint
-- on the next evaluation under the older engine).

DELETE FROM evidence WHERE authority = 'control_plane';

ALTER TABLE evidence DROP CONSTRAINT IF EXISTS evidence_authority_check;

ALTER TABLE evidence ADD CONSTRAINT evidence_authority_check
    CHECK (authority IN ('diagnostic', 'merge_gate'));
