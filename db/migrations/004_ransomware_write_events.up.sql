CREATE TABLE ransomware_write_events (
  id BIGSERIAL PRIMARY KEY,
  volume_id BIGINT NOT NULL REFERENCES volumes(id),
  file_path TEXT NOT NULL,
  before_seq BIGINT NOT NULL,
  previous_entropy REAL,
  current_entropy REAL NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ransomware_write_events_window_idx
  ON ransomware_write_events (volume_id, created_at);
