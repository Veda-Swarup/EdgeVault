-- Path-scoped leases can be acquired before the first file version exists.
ALTER TABLE leases ADD COLUMN volume_id BIGINT;
ALTER TABLE leases ADD COLUMN file_path TEXT;

UPDATE leases l
SET volume_id = f.volume_id, file_path = f.path
FROM files f
WHERE f.id = l.file_id;

DELETE FROM leases WHERE volume_id IS NULL OR file_path IS NULL;
ALTER TABLE leases DROP CONSTRAINT leases_pkey;
ALTER TABLE leases DROP COLUMN file_id;
ALTER TABLE leases ALTER COLUMN volume_id SET NOT NULL;
ALTER TABLE leases ALTER COLUMN file_path SET NOT NULL;
ALTER TABLE leases ADD CONSTRAINT leases_pkey PRIMARY KEY (volume_id, file_path);
ALTER TABLE leases ADD CONSTRAINT leases_volume_fk FOREIGN KEY (volume_id) REFERENCES volumes(id) ON DELETE CASCADE;
CREATE INDEX leases_expiry_idx ON leases (expires_at);
