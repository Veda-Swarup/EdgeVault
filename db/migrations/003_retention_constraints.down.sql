DROP INDEX IF EXISTS snapshots_protected_seq_idx;
DROP INDEX IF EXISTS chunks_gc_age_idx;
ALTER TABLE volumes DROP CONSTRAINT IF EXISTS volumes_retention_policy_ck;
