package meta

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type DashboardFile struct {
	Path      string    `json:"path"`
	Version   int64     `json:"current_version"`
	Size      int64     `json:"size"`
	Deleted   bool      `json:"deleted"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DashboardSnapshot struct {
	ID        int64     `json:"id"`
	CommitSeq int64     `json:"commit_seq"`
	Protected bool      `json:"protected"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type Dashboard struct {
	Files        []DashboardFile     `json:"files"`
	Snapshots    []DashboardSnapshot `json:"snapshots"`
	CurrentBytes int64               `json:"current_bytes"`
	StoredBytes  int64               `json:"stored_bytes"`
	ChunkCount   int64               `json:"chunk_count"`
	VersionCount int64               `json:"version_count"`
}

// Dashboard reads all totals from one consistent database snapshot. StoredBytes
// includes tracked chunks for retained history and chunks awaiting collection.
func (s *Store) Dashboard(ctx context.Context, volumeID int64) (Dashboard, error) {
	d := Dashboard{Files: []DashboardFile{}, Snapshots: []DashboardSnapshot{}}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, `SELECT f.path,f.current_version,v.size,v.is_deleted,f.updated_at FROM files f JOIN file_versions v ON v.file_id=f.id AND v.version=f.current_version WHERE f.volume_id=$1 ORDER BY f.path`, volumeID)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var f DashboardFile
		if err = rows.Scan(&f.Path, &f.Version, &f.Size, &f.Deleted, &f.UpdatedAt); err != nil {
			rows.Close()
			return d, err
		}
		d.Files = append(d.Files, f)
		if !f.Deleted {
			d.CurrentBytes += f.Size
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(stored_size),0),COUNT(*) FROM chunks WHERE volume_id=$1`, volumeID).Scan(&d.StoredBytes, &d.ChunkCount); err != nil {
		return d, err
	}
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM file_versions WHERE volume_id=$1`, volumeID).Scan(&d.VersionCount); err != nil {
		return d, err
	}
	rows, err = tx.Query(ctx, `SELECT id,commit_seq,protected,reason,created_at FROM snapshots WHERE volume_id=$1 ORDER BY id DESC LIMIT 100`, volumeID)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var snap DashboardSnapshot
		if err = rows.Scan(&snap.ID, &snap.CommitSeq, &snap.Protected, &snap.Reason, &snap.CreatedAt); err != nil {
			rows.Close()
			return d, err
		}
		d.Snapshots = append(d.Snapshots, snap)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}
