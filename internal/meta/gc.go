package meta

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const expiredVersionsSQL = `WITH ranked AS (
  SELECT fv.file_id, fv.version, fv.volume_id, fv.commit_seq, fv.created_at,
         f.current_version, v.keep_days, v.floor_days, v.min_versions,
         row_number() OVER (PARTITION BY fv.file_id ORDER BY fv.version DESC) AS version_rank
  FROM file_versions fv
  JOIN files f ON f.id=fv.file_id
  JOIN volumes v ON v.id=fv.volume_id
  WHERE fv.volume_id=$1
), expired AS (
  SELECT r.file_id,r.version FROM ranked r
  WHERE r.version<>r.current_version
    AND r.version_rank>r.min_versions
    AND r.created_at < now()-GREATEST(r.keep_days,r.floor_days)*interval '1 day'
    AND NOT EXISTS (
      SELECT 1 FROM snapshots s
      WHERE s.volume_id=r.volume_id AND s.protected=true
        AND r.commit_seq=(SELECT max(v2.commit_seq) FROM file_versions v2
                          WHERE v2.file_id=r.file_id AND v2.commit_seq<=s.commit_seq)
    )
) `

const noRetainedChunkReferencesSQL = `NOT EXISTS (
  SELECT 1 FROM version_chunks vc
  JOIN file_versions fv ON fv.file_id=vc.file_id AND fv.version=vc.version
  JOIN files f ON f.id=fv.file_id
  JOIN volumes v ON v.id=fv.volume_id
  WHERE fv.volume_id=c.volume_id AND vc.chunk_id=c.chunk_id
    AND (
      fv.version=f.current_version
      OR (SELECT count(*) FROM file_versions newer WHERE newer.file_id=fv.file_id AND newer.version>fv.version)<v.min_versions
      OR fv.created_at>=now()-GREATEST(v.keep_days,v.floor_days)*interval '1 day'
      OR EXISTS(SELECT 1 FROM snapshots s WHERE s.volume_id=fv.volume_id AND s.protected=true
        AND fv.commit_seq=(SELECT max(v2.commit_seq) FROM file_versions v2 WHERE v2.file_id=fv.file_id AND v2.commit_seq<=s.commit_seq))
    )
)`

// ExpireOldVersions deletes only rows outside all retention guarantees. The
// volume row lock serializes policy changes and prevents GC during alerts.
func (s *Store) ExpireOldVersions(ctx context.Context, volumeID int64, dryRun bool) (deleted int64, skipped bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var alertOpen bool
	if err = tx.QueryRow(ctx, `SELECT alert_open FROM volumes WHERE id=$1 FOR UPDATE`, volumeID).Scan(&alertOpen); err != nil {
		return 0, false, err
	}
	if alertOpen {
		return 0, true, nil
	}
	if dryRun {
		err = tx.QueryRow(ctx, expiredVersionsSQL+`SELECT count(*) FROM expired`, volumeID).Scan(&deleted)
		return deleted, false, err
	}
	if _, err = tx.Exec(ctx, expiredVersionsSQL+`DELETE FROM version_chunks vc USING expired e WHERE vc.file_id=e.file_id AND vc.version=e.version`, volumeID); err != nil {
		return 0, false, err
	}
	tag, err := tx.Exec(ctx, expiredVersionsSQL+`DELETE FROM file_versions fv USING expired e WHERE fv.file_id=e.file_id AND fv.version=e.version`, volumeID)
	if err != nil {
		return 0, false, err
	}
	deleted = tag.RowsAffected()
	if deleted > 0 {
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(volume_id,actor,kind,detail) VALUES($1,'gc','retention_expired',jsonb_build_object('versions_deleted',$2::bigint))`, volumeID, deleted); err != nil {
			return 0, false, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return deleted, false, nil
}

func (s *Store) ListStaleChunks(ctx context.Context, volumeID int64, cutoff time.Time) ([]ChunkMeta, error) {
	query := `SELECT c.volume_id,c.chunk_id,c.size,c.stored_size,c.compression,c.s3_key,c.created_at,c.last_used_at FROM chunks c JOIN volumes v ON v.id=c.volume_id WHERE c.volume_id=$1 AND v.alert_open=false AND c.last_used_at<$2 AND ` + noRetainedChunkReferencesSQL + ` ORDER BY c.last_used_at`
	rows, err := s.db.Query(ctx, query, volumeID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkMeta
	for rows.Next() {
		var c ChunkMeta
		if err := rows.Scan(&c.VolumeID, &c.ChunkID, &c.Size, &c.StoredSize, &c.Compression, &c.S3Key, &c.CreatedAt, &c.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SweepChunk locks the volume and chunk row, rechecks the alert/reference/age
// predicates, then deletes S3 data while a dedup writer's TouchChunk is blocked.
func (s *Store) SweepChunk(ctx context.Context, volumeID int64, chunkID string, cutoff time.Time, dryRun bool, deleteObject func(context.Context, string) error) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var alert bool
	if err = tx.QueryRow(ctx, `SELECT alert_open FROM volumes WHERE id=$1 FOR UPDATE`, volumeID).Scan(&alert); err != nil {
		return false, err
	}
	if alert {
		return false, nil
	}
	var key string
	query := `SELECT c.s3_key FROM chunks c WHERE c.volume_id=$1 AND c.chunk_id=$2 AND c.last_used_at<$3 AND ` + noRetainedChunkReferencesSQL + ` FOR UPDATE OF c`
	err = tx.QueryRow(ctx, query, volumeID, chunkID, cutoff).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if dryRun {
		return true, nil
	}
	if deleteObject == nil {
		return false, fmt.Errorf("gc: delete callback is required outside dry-run")
	}
	if err = deleteObject(ctx, key); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chunks WHERE volume_id=$1 AND chunk_id=$2`, volumeID, chunkID); err != nil {
		return false, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(volume_id,actor,kind,detail) VALUES($1,'gc','chunk_swept',jsonb_build_object('chunk_id',$2::text))`, volumeID, chunkID); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
