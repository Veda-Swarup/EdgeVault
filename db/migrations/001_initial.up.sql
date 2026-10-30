-- 001_initial.up.sql
-- EdgeVault initial schema.
-- Design note: volumes carry the head_seq counter so every committed
-- version gets a monotonically increasing number without a global sequence.

CREATE TABLE volumes (
  id               BIGSERIAL PRIMARY KEY,
  name             TEXT UNIQUE NOT NULL,
  kms_key_id       TEXT NOT NULL,
  wrapped_data_key BYTEA NOT NULL,
  key_version      INT NOT NULL DEFAULT 1,
  head_seq         BIGINT NOT NULL DEFAULT 0,
  keep_days        INT NOT NULL DEFAULT 30,
  min_versions     INT NOT NULL DEFAULT 3,
  floor_days       INT NOT NULL DEFAULT 7,
  alert_open       BOOLEAN NOT NULL DEFAULT false,
  chunker          TEXT NOT NULL DEFAULT 'fixed-4MiB',
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE files (
  id              BIGSERIAL PRIMARY KEY,
  volume_id       BIGINT NOT NULL REFERENCES volumes(id),
  path            TEXT NOT NULL,
  current_version BIGINT NOT NULL DEFAULT 0,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (volume_id, path)
);

CREATE TABLE file_versions (
  file_id       BIGINT NOT NULL REFERENCES files(id),
  version       BIGINT NOT NULL,
  volume_id     BIGINT NOT NULL,
  commit_seq    BIGINT NOT NULL,
  size          BIGINT NOT NULL,
  manifest_hash BYTEA NOT NULL,
  is_deleted    BOOLEAN NOT NULL DEFAULT false,
  avg_entropy   REAL,
  created_by    TEXT NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (file_id, version)
);
CREATE INDEX ON file_versions (volume_id, commit_seq);

CREATE TABLE version_chunks (
  file_id  BIGINT NOT NULL,
  version  BIGINT NOT NULL,
  idx      INT NOT NULL,
  chunk_id TEXT NOT NULL,
  size     INT NOT NULL,
  "offset" BIGINT NOT NULL,
  PRIMARY KEY (file_id, version, idx)
);
CREATE INDEX ON version_chunks (chunk_id);

CREATE TABLE chunks (
  volume_id    BIGINT NOT NULL,
  chunk_id     TEXT NOT NULL,
  size         INT NOT NULL,
  stored_size  INT NOT NULL,
  compression  SMALLINT NOT NULL DEFAULT 0,
  s3_key       TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (volume_id, chunk_id)
);

CREATE TABLE leases (
  file_id    BIGINT PRIMARY KEY,
  holder     TEXT NOT NULL,
  token      BIGINT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE snapshots (
  id         BIGSERIAL PRIMARY KEY,
  volume_id  BIGINT NOT NULL,
  commit_seq BIGINT NOT NULL,
  protected  BOOLEAN NOT NULL DEFAULT false,
  reason     TEXT NOT NULL,
  s3_key     TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
  id         BIGSERIAL PRIMARY KEY,
  volume_id  BIGINT,
  actor      TEXT,
  kind       TEXT NOT NULL,
  detail     JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
