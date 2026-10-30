ALTER TABLE volumes
  ADD CONSTRAINT volumes_retention_policy_ck
    CHECK (floor_days >= 7 AND keep_days >= floor_days AND min_versions >= 1);

CREATE INDEX chunks_gc_age_idx ON chunks (volume_id, last_used_at);
CREATE INDEX snapshots_protected_seq_idx ON snapshots (volume_id, commit_seq)
  WHERE protected = true;
