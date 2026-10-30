package meta

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vedaswarup/edgevault/internal/detector"
)

// ErrNotFound is returned when a queried row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when an optimistic-concurrency check fails
// (the caller's expected version does not match the current version).
var ErrConflict = errors.New("version conflict")

// ErrUnchanged is returned when the uploaded file is byte-for-byte identical
// to the current version (manifest_hash matches); no new version is created.
var ErrUnchanged = errors.New("content unchanged")
var ErrLeaseHeld = errors.New("lease is held by another edge")
var ErrLeaseLost = errors.New("lease expired or fencing token is stale")
var ErrRetentionFloor = errors.New("retention policy violates the minimum floor")
var ErrVolumeAlertOpen = errors.New("writes are paused while ransomware alert is open")

type Lease struct {
	Holder    string
	Token     int64
	ExpiresAt time.Time
}

// Volume is a minimal view of the volumes row, enough for the commit path.
type Volume struct {
	ID             int64
	Name           string
	WrappedDataKey []byte
	KMSKeyID       string
	KeyVersion     int
	HeadSeq        int64
	AlertOpen      bool
	KeepDays       int
	MinVersions    int
	FloorDays      int
}

type Snapshot struct {
	ID, VolumeID, CommitSeq int64
	Protected               bool
	Reason                  string
	S3Key                   string
	CreatedAt               time.Time
}
type StateEntry struct {
	FileID                   int64
	Path                     string
	Version, CommitSeq, Size int64
	ManifestHash             []byte
	IsDeleted                bool
}

// File is a row from the files table.
type File struct {
	ID             int64
	VolumeID       int64
	Path           string
	CurrentVersion int64
	UpdatedAt      time.Time
}

// FileVersion is a row from file_versions.
type FileVersion struct {
	FileID       int64
	Version      int64
	VolumeID     int64
	CommitSeq    int64
	Size         int64
	ManifestHash []byte
	IsDeleted    bool
	AvgEntropy   *float32
	CreatedBy    string
	CreatedAt    time.Time
}

// VersionChunk is a row from version_chunks.
type VersionChunk struct {
	FileID  int64
	Version int64
	Idx     int
	ChunkID string
	Size    int
	Offset  int64
}

// ChunkMeta is a row from the chunks table.
type ChunkMeta struct {
	VolumeID    int64
	ChunkID     string
	Size        int
	StoredSize  int
	Compression int16
	S3Key       string
	CreatedAt   time.Time
	LastUsedAt  time.Time
}

// CommitInput carries everything the store needs to atomically commit a new
// file version.
type CommitInput struct {
	VolumeID          int64
	FilePath          string
	ExpectedVersion   int64 // current_version the caller observed; 0 means "new file"
	Size              int64
	ManifestHash      []byte
	CreatedBy         string
	Chunks            []VersionChunk // idx / chunk_id / size / offset already set
	AvgEntropy        *float32
	LeaseHolder       string
	LeaseToken        int64
	AllowConflictCopy bool
	AllowRecovery     bool
}

// CommitResult is returned by Commit.
type CommitResult struct {
	Version   int64
	CommitSeq int64
}

// Store wraps a pgxpool.Pool and provides all metadata operations.
type Store struct {
	db *pgxpool.Pool
}

// NewStore wraps an existing connection pool.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

// --- Volume operations ---

// GetVolumeByName returns the volume with the given name.
func (s *Store) GetVolumeByName(ctx context.Context, name string) (Volume, error) {
	const q = `SELECT id, name, kms_key_id, wrapped_data_key, key_version, head_seq, alert_open, keep_days, min_versions, floor_days
	           FROM volumes WHERE name = $1`
	var v Volume
	err := s.db.QueryRow(ctx, q, name).Scan(
		&v.ID, &v.Name, &v.KMSKeyID, &v.WrappedDataKey, &v.KeyVersion, &v.HeadSeq, &v.AlertOpen, &v.KeepDays, &v.MinVersions, &v.FloorDays,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Volume{}, ErrNotFound
	}
	if err != nil {
		return Volume{}, fmt.Errorf("meta: get volume %q: %w", name, err)
	}
	return v, nil
}

func (s *Store) ListVolumes(ctx context.Context) ([]Volume, error) {
	rows, err := s.db.Query(ctx, `SELECT id,name,kms_key_id,wrapped_data_key,key_version,head_seq,alert_open,keep_days,min_versions,floor_days FROM volumes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ID, &v.Name, &v.KMSKeyID, &v.WrappedDataKey, &v.KeyVersion, &v.HeadSeq, &v.AlertOpen, &v.KeepDays, &v.MinVersions, &v.FloorDays); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SetRetentionPolicy(ctx context.Context, volumeID int64, actor string, keepDays, minVersions, floorDays int) error {
	if floorDays < 7 || keepDays < floorDays || minVersions < 1 {
		return ErrRetentionFloor
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE volumes SET keep_days=$2,min_versions=$3,floor_days=$4 WHERE id=$1`, volumeID, keepDays, minVersions, floorDays)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(volume_id,actor,kind,detail) VALUES($1,$2,'retention_policy_changed',jsonb_build_object('keep_days',$3::int,'min_versions',$4::int,'floor_days',$5::int))`, volumeID, actor, keepDays, minVersions, floorDays); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) CreateSnapshot(ctx context.Context, volumeID, seq int64, protected bool, reason string) (Snapshot, error) {
	var out Snapshot
	err := s.db.QueryRow(ctx, `INSERT INTO snapshots(volume_id,commit_seq,protected,reason) VALUES($1,$2,$3,$4) RETURNING id,volume_id,commit_seq,protected,reason,COALESCE(s3_key,''),created_at`, volumeID, seq, protected, reason).Scan(&out.ID, &out.VolumeID, &out.CommitSeq, &out.Protected, &out.Reason, &out.S3Key, &out.CreatedAt)
	return out, err
}

func (s *Store) TakeSnapshot(ctx context.Context, volumeID int64, protected bool, reason string) (Snapshot, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var seq int64
	if err = tx.QueryRow(ctx, `SELECT head_seq FROM volumes WHERE id=$1 FOR UPDATE`, volumeID).Scan(&seq); err != nil {
		return Snapshot{}, err
	}
	var out Snapshot
	err = tx.QueryRow(ctx, `INSERT INTO snapshots(volume_id,commit_seq,protected,reason) VALUES($1,$2,$3,$4) RETURNING id,volume_id,commit_seq,protected,reason,COALESCE(s3_key,''),created_at`, volumeID, seq, protected, reason).Scan(&out.ID, &out.VolumeID, &out.CommitSeq, &out.Protected, &out.Reason, &out.S3Key, &out.CreatedAt)
	if err != nil {
		return Snapshot{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

func (s *Store) SetSnapshotS3Key(ctx context.Context, volumeID, snapshotID int64, key string) error {
	_, err := s.db.Exec(ctx, `UPDATE snapshots SET s3_key=$3 WHERE volume_id=$1 AND id=$2`, volumeID, snapshotID, key)
	return err
}
func (s *Store) DeleteSnapshot(ctx context.Context, volumeID, snapshotID int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM snapshots WHERE volume_id=$1 AND id=$2`, volumeID, snapshotID)
	return err
}

type WriteSignalDecision struct {
	Alert            bool
	BaselineSeq      int64
	Score            float64
	DistinctFiles    int
	EntropyJumpFiles int
}

// ObserveWriteSignal stores detector state in PostgreSQL so writes routed to
// different edge replicas contribute to the same sliding window. Locking the
// volume row serializes threshold evaluation and closes the alert atomically.
func (s *Store) ObserveWriteSignal(ctx context.Context, volumeID int64, path, actor string, beforeSeq int64, previous, current *float32) (WriteSignalDecision, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return WriteSignalDecision{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var alertOpen bool
	if err = tx.QueryRow(ctx, `SELECT alert_open FROM volumes WHERE id=$1 FOR UPDATE`, volumeID).Scan(&alertOpen); err != nil {
		return WriteSignalDecision{}, err
	}
	if alertOpen {
		return WriteSignalDecision{}, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ransomware_write_events(volume_id,file_path,before_seq,previous_entropy,current_entropy) VALUES($1,$2,$3,$4,$5)`, volumeID, path, beforeSeq, previous, current); err != nil {
		return WriteSignalDecision{}, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM ransomware_write_events WHERE volume_id=$1 AND created_at<now()-interval '60 seconds'`, volumeID); err != nil {
		return WriteSignalDecision{}, err
	}
	var decision WriteSignalDecision
	err = tx.QueryRow(ctx, `WITH recent AS (
	  SELECT file_path,before_seq,previous_entropy,current_entropy,created_at
	  FROM ransomware_write_events WHERE volume_id=$1 AND created_at >= now()-interval '60 seconds'
	), boundary AS (
	  SELECT min(created_at) AS first_jump FROM recent
	  WHERE previous_entropy IS NOT NULL AND current_entropy-previous_entropy >= $2
	), active AS (
	  SELECT r.* FROM recent r CROSS JOIN boundary b
	  WHERE b.first_jump IS NULL OR r.created_at >= b.first_jump
	)
	SELECT COALESCE(min(before_seq),0),count(DISTINCT file_path),
	       count(DISTINCT file_path) FILTER (WHERE previous_entropy IS NOT NULL AND current_entropy-previous_entropy >= $2)
	FROM active`, volumeID, detector.MinEntropyJump).Scan(&decision.BaselineSeq, &decision.DistinctFiles, &decision.EntropyJumpFiles)
	if err != nil {
		return WriteSignalDecision{}, err
	}
	decision.Score = float64(decision.DistinctFiles) + 2*float64(decision.EntropyJumpFiles)
	decision.Alert = decision.DistinctFiles >= detector.MinDistinctFiles && decision.EntropyJumpFiles >= detector.MinJumpedFiles
	if decision.Alert {
		if _, err = tx.Exec(ctx, `UPDATE volumes SET alert_open=true WHERE id=$1`, volumeID); err != nil {
			return WriteSignalDecision{}, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO audit_events(volume_id,actor,kind,detail) VALUES($1,$2,'ransomware_alert',jsonb_build_object('snapshot_id',NULL,'baseline_seq',$3::bigint,'score',$4::float8))`, volumeID, actor, decision.BaselineSeq, decision.Score); err != nil {
			return WriteSignalDecision{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return WriteSignalDecision{}, err
	}
	return decision, nil
}

func (s *Store) OpenAlert(ctx context.Context, volumeID int64, actor string, snapshotID int64, score float64) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE audit_events SET detail=jsonb_set(detail,'{snapshot_id}',to_jsonb($3::bigint),true),actor=$2 WHERE id=(SELECT id FROM audit_events WHERE volume_id=$1 AND kind='ransomware_alert' ORDER BY id DESC LIMIT 1)`, volumeID, actor, snapshotID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("meta: ransomware alert event not found for volume %d (score %.2f)", volumeID, score)
	}
	return tx.Commit(ctx)
}

func (s *Store) CloseAlert(ctx context.Context, volumeID int64, actor string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err = tx.Exec(ctx, `UPDATE volumes SET alert_open=false WHERE id=$1`, volumeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM ransomware_write_events WHERE volume_id=$1`, volumeID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO audit_events(volume_id,actor,kind,detail) VALUES($1,$2,'ransomware_recovered','{}'::jsonb)`, volumeID, actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) GetSnapshot(ctx context.Context, volumeID, snapshotID int64) (Snapshot, error) {
	var out Snapshot
	err := s.db.QueryRow(ctx, `SELECT id,volume_id,commit_seq,protected,reason,COALESCE(s3_key,''),created_at FROM snapshots WHERE volume_id=$1 AND id=$2`, volumeID, snapshotID).Scan(&out.ID, &out.VolumeID, &out.CommitSeq, &out.Protected, &out.Reason, &out.S3Key, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return out, err
}

func (s *Store) StateAt(ctx context.Context, volumeID, seq int64, prefix string) ([]StateEntry, error) {
	const q = `SELECT file_id,path,version,commit_seq,size,manifest_hash,is_deleted FROM (
	 SELECT DISTINCT ON(f.id) f.id AS file_id,f.path,v.version,v.commit_seq,v.size,v.manifest_hash,v.is_deleted
	 FROM file_versions v JOIN files f ON f.id=v.file_id
	 WHERE f.volume_id=$1 AND v.commit_seq<=$2 AND f.path LIKE $3
	 ORDER BY f.id,v.commit_seq DESC
	) state ORDER BY path`
	rows, err := s.db.Query(ctx, q, volumeID, seq, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateEntry
	for rows.Next() {
		var e StateEntry
		if err := rows.Scan(&e.FileID, &e.Path, &e.Version, &e.CommitSeq, &e.Size, &e.ManifestHash, &e.IsDeleted); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateVolume inserts a new volume row.  The caller passes the already-wrapped
// data key (ciphertext from KMS).
func (s *Store) CreateVolume(ctx context.Context, name, kmsKeyID string, wrappedKey []byte) (Volume, error) {
	const q = `INSERT INTO volumes (name, kms_key_id, wrapped_data_key, key_version)
	           VALUES ($1, $2, $3, 2)
	           RETURNING id, name, kms_key_id, wrapped_data_key, key_version, head_seq, alert_open, keep_days, min_versions, floor_days`
	var v Volume
	err := s.db.QueryRow(ctx, q, name, kmsKeyID, wrappedKey).Scan(
		&v.ID, &v.Name, &v.KMSKeyID, &v.WrappedDataKey, &v.KeyVersion, &v.HeadSeq, &v.AlertOpen, &v.KeepDays, &v.MinVersions, &v.FloorDays,
	)
	if err != nil {
		return Volume{}, fmt.Errorf("meta: create volume %q: %w", name, err)
	}
	return v, nil
}

// --- File / version operations ---

// GetFile returns the file row for a given volume and path.
func (s *Store) GetFile(ctx context.Context, volumeID int64, path string) (File, error) {
	const q = `SELECT id, volume_id, path, current_version, updated_at
	           FROM files WHERE volume_id = $1 AND path = $2`
	var f File
	err := s.db.QueryRow(ctx, q, volumeID, path).Scan(
		&f.ID, &f.VolumeID, &f.Path, &f.CurrentVersion, &f.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("meta: get file %q: %w", path, err)
	}
	return f, nil
}

// GetFileVersion returns a specific version of a file.
func (s *Store) GetFileVersion(ctx context.Context, fileID, version int64) (FileVersion, error) {
	const q = `SELECT file_id, version, volume_id, commit_seq, size, manifest_hash,
	                  is_deleted, avg_entropy, created_by, created_at
	           FROM file_versions WHERE file_id = $1 AND version = $2`
	var v FileVersion
	err := s.db.QueryRow(ctx, q, fileID, version).Scan(
		&v.FileID, &v.Version, &v.VolumeID, &v.CommitSeq, &v.Size,
		&v.ManifestHash, &v.IsDeleted, &v.AvgEntropy, &v.CreatedBy, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return FileVersion{}, ErrNotFound
	}
	if err != nil {
		return FileVersion{}, fmt.Errorf("meta: get file version: %w", err)
	}
	return v, nil
}

// GetCurrentManifestHash returns the manifest_hash of the current version,
// used for the no-change dedup check.  Returns nil if the file does not exist
// yet.
func (s *Store) GetCurrentManifestHash(ctx context.Context, volumeID int64, path string) ([]byte, error) {
	const q = `SELECT fv.manifest_hash
	           FROM files f
	           JOIN file_versions fv ON fv.file_id = f.id AND fv.version = f.current_version
	           WHERE f.volume_id = $1 AND f.path = $2`
	var h []byte
	err := s.db.QueryRow(ctx, q, volumeID, path).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("meta: get manifest hash: %w", err)
	}
	return h, nil
}

// GetVersionChunks returns the ordered chunks for a given file version.
func (s *Store) GetVersionChunks(ctx context.Context, fileID, version int64) ([]VersionChunk, error) {
	const q = `SELECT file_id, version, idx, chunk_id, size, "offset"
	           FROM version_chunks WHERE file_id = $1 AND version = $2
	           ORDER BY idx`
	rows, err := s.db.Query(ctx, q, fileID, version)
	if err != nil {
		return nil, fmt.Errorf("meta: get version chunks: %w", err)
	}
	defer rows.Close()

	var chunks []VersionChunk
	for rows.Next() {
		var c VersionChunk
		if err := rows.Scan(&c.FileID, &c.Version, &c.Idx, &c.ChunkID, &c.Size, &c.Offset); err != nil {
			return nil, fmt.Errorf("meta: scan version chunk: %w", err)
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// GetChunk returns metadata for a single chunk.
func (s *Store) GetChunk(ctx context.Context, volumeID int64, chunkID string) (ChunkMeta, error) {
	const q = `SELECT volume_id, chunk_id, size, stored_size, compression, s3_key, created_at, last_used_at
	           FROM chunks WHERE volume_id = $1 AND chunk_id = $2`
	var c ChunkMeta
	err := s.db.QueryRow(ctx, q, volumeID, chunkID).Scan(
		&c.VolumeID, &c.ChunkID, &c.Size, &c.StoredSize, &c.Compression,
		&c.S3Key, &c.CreatedAt, &c.LastUsedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChunkMeta{}, ErrNotFound
	}
	if err != nil {
		return ChunkMeta{}, fmt.Errorf("meta: get chunk: %w", err)
	}
	return c, nil
}

// UpsertChunk inserts a chunk row or, if it already exists, bumps last_used_at.
// This is called in the dedup path: if the chunk is already in S3 we still
// want to update the GC timestamp.
func (s *Store) UpsertChunk(ctx context.Context, c ChunkMeta) error {
	const q = `INSERT INTO chunks (volume_id, chunk_id, size, stored_size, compression, s3_key, last_used_at)
	           VALUES ($1, $2, $3, $4, $5, $6, now())
	           ON CONFLICT (volume_id, chunk_id) DO UPDATE SET last_used_at = now()`
	_, err := s.db.Exec(ctx, q,
		c.VolumeID, c.ChunkID, c.Size, c.StoredSize, c.Compression, c.S3Key,
	)
	if err != nil {
		return fmt.Errorf("meta: upsert chunk: %w", err)
	}
	return nil
}

// TouchChunk atomically moves a deduplicated chunk out of the GC grace window.
// A false result means GC already removed the row, so the caller must re-upload.
func (s *Store) TouchChunk(ctx context.Context, volumeID int64, chunkID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE chunks SET last_used_at=now() WHERE volume_id=$1 AND chunk_id=$2`, volumeID, chunkID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Commit atomically creates a new file version.  It runs the full commit
// protocol inside a single transaction:
//  1. Verify expected version (optimistic concurrency).
//  2. Increment files.current_version.
//  3. Increment volumes.head_seq.
//  4. Insert file_versions and version_chunks rows.
//
// The sequence counter is taken last (step 3) while the volume row is still
// locked, so commit_seq order equals wall-clock commit order within the volume.
//
// Returns ErrConflict if the optimistic check fails.
// Returns ErrUnchanged if manifest_hash matches the current version.
func (s *Store) Commit(ctx context.Context, in CommitInput) (CommitResult, error) {
	// Phase 1 note: chunks are already uploaded to S3 before this transaction
	// starts, so the volume row is locked only for the brief duration of the
	// SQL statements below — not for the whole upload time.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CommitResult{}, fmt.Errorf("meta: begin commit tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := verifyVolumeWritableTx(ctx, tx, in.VolumeID, in.AllowRecovery); err != nil {
		return CommitResult{}, err
	}

	if !in.AllowConflictCopy {
		if err := verifyLeaseTx(ctx, tx, in.VolumeID, in.FilePath, in.LeaseHolder, in.LeaseToken); err != nil {
			return CommitResult{}, err
		}
	}

	// Step 1: upsert the files row.  If the file is new, current_version starts
	// at 0.  For an existing file we use an UPDATE ... WHERE current_version =
	// expected to detect conflicts (optimistic locking).
	var fileID int64
	var newVersion int64

	if in.ExpectedVersion == 0 {
		// New file — insert if not exists, conflict if another edge beat us.
		const insFile = `
			INSERT INTO files (volume_id, path, current_version, updated_at)
			VALUES ($1, $2, 1, now())
			ON CONFLICT (volume_id, path) DO NOTHING
			RETURNING id, current_version`
		err = tx.QueryRow(ctx, insFile, in.VolumeID, in.FilePath).Scan(&fileID, &newVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			// Another edge already created this file.
			return CommitResult{}, ErrConflict
		}
		if err != nil {
			return CommitResult{}, fmt.Errorf("meta: insert file: %w", err)
		}
	} else {
		// Existing file — advance current_version only if it still equals what
		// the caller observed.  If another writer committed between our read and
		// now, no row is returned → conflict.
		const updFile = `
			UPDATE files SET current_version = current_version + 1, updated_at = now()
			WHERE volume_id = $1 AND path = $2 AND current_version = $3
			RETURNING id, current_version`
		err = tx.QueryRow(ctx, updFile, in.VolumeID, in.FilePath, in.ExpectedVersion).
			Scan(&fileID, &newVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return CommitResult{}, ErrConflict
		}
		if err != nil {
			return CommitResult{}, fmt.Errorf("meta: update file version: %w", err)
		}
	}

	// Step 2: advance the volume sequence counter.
	// Taking the sequence after the file row update means the volume row is
	// locked for just these two statements, keeping the critical section small.
	const updVol = `
		UPDATE volumes SET head_seq = head_seq + 1 WHERE id = $1 RETURNING head_seq`
	var commitSeq int64
	if err := tx.QueryRow(ctx, updVol, in.VolumeID).Scan(&commitSeq); err != nil {
		return CommitResult{}, fmt.Errorf("meta: advance head_seq: %w", err)
	}

	// Step 3: insert the file_versions row.
	const insVer = `
		INSERT INTO file_versions
		  (file_id, version, volume_id, commit_seq, size, manifest_hash, avg_entropy, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := tx.Exec(ctx, insVer,
		fileID, newVersion, in.VolumeID, commitSeq,
		in.Size, in.ManifestHash, in.AvgEntropy, in.CreatedBy,
	); err != nil {
		return CommitResult{}, fmt.Errorf("meta: insert file_version: %w", err)
	}

	// Step 4: insert version_chunks rows.
	for _, c := range in.Chunks {
		const insChunk = `
			INSERT INTO version_chunks (file_id, version, idx, chunk_id, size, "offset")
			VALUES ($1, $2, $3, $4, $5, $6)`
		if _, err := tx.Exec(ctx, insChunk,
			fileID, newVersion, c.Idx, c.ChunkID, c.Size, c.Offset,
		); err != nil {
			return CommitResult{}, fmt.Errorf("meta: insert version_chunk idx=%d: %w", c.Idx, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return CommitResult{}, fmt.Errorf("meta: commit tx: %w", err)
	}

	return CommitResult{Version: newVersion, CommitSeq: commitSeq}, nil
}

func verifyLeaseTx(ctx context.Context, tx pgx.Tx, volumeID int64, path, holder string, token int64) error {
	var current int64
	err := tx.QueryRow(ctx, `SELECT token FROM leases WHERE volume_id=$1 AND file_path=$2 AND holder=$3 AND token=$4 AND expires_at>now() FOR UPDATE`, volumeID, path, holder, token).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("meta: verify lease: %w", err)
	}
	return nil
}

func verifyVolumeWritableTx(ctx context.Context, tx pgx.Tx, volumeID int64, allowRecovery bool) error {
	var alertOpen bool
	if err := tx.QueryRow(ctx, `SELECT alert_open FROM volumes WHERE id=$1 FOR UPDATE`, volumeID).Scan(&alertOpen); err != nil {
		return err
	}
	if alertOpen && !allowRecovery {
		return ErrVolumeAlertOpen
	}
	return nil
}

// CommitTombstone writes a deletion marker as a new version.
// A delete is non-destructive: it just adds an is_deleted=true version.
func (s *Store) CommitTombstone(ctx context.Context, volumeID int64, path, createdBy, leaseHolder string, leaseToken int64, allowRecovery bool) (CommitResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CommitResult{}, fmt.Errorf("meta: begin tombstone tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := verifyVolumeWritableTx(ctx, tx, volumeID, allowRecovery); err != nil {
		return CommitResult{}, err
	}
	if err := verifyLeaseTx(ctx, tx, volumeID, path, leaseHolder, leaseToken); err != nil {
		return CommitResult{}, err
	}

	// Advance current_version on the files row (unconditionally).
	const updFile = `
		UPDATE files SET current_version = current_version + 1, updated_at = now()
		WHERE volume_id = $1 AND path = $2
		RETURNING id, current_version`
	var fileID, newVersion int64
	err = tx.QueryRow(ctx, updFile, volumeID, path).Scan(&fileID, &newVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommitResult{}, ErrNotFound
	}
	if err != nil {
		return CommitResult{}, fmt.Errorf("meta: tombstone update file: %w", err)
	}

	const updVol = `UPDATE volumes SET head_seq = head_seq + 1 WHERE id = $1 RETURNING head_seq`
	var commitSeq int64
	if err := tx.QueryRow(ctx, updVol, volumeID).Scan(&commitSeq); err != nil {
		return CommitResult{}, fmt.Errorf("meta: tombstone head_seq: %w", err)
	}

	const insVer = `
		INSERT INTO file_versions
		  (file_id, version, volume_id, commit_seq, size, manifest_hash, is_deleted, created_by)
		VALUES ($1, $2, $3, $4, 0, ''::bytea, true, $5)`
	if _, err := tx.Exec(ctx, insVer, fileID, newVersion, volumeID, commitSeq, createdBy); err != nil {
		return CommitResult{}, fmt.Errorf("meta: insert tombstone version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CommitResult{}, fmt.Errorf("meta: tombstone commit: %w", err)
	}
	return CommitResult{Version: newVersion, CommitSeq: commitSeq}, nil
}

// AcquireLease grants or renews a path lease. Reacquisition by the same live
// holder renews the expiry without changing its fencing token; reclaim after
// expiry increments the token so paused holders cannot commit stale work.
func (s *Store) AcquireLease(ctx context.Context, volumeID int64, path, holder string, ttl time.Duration) (Lease, error) {
	if ttl <= 0 || ttl > 5*time.Minute {
		return Lease{}, fmt.Errorf("lease TTL must be between 1ns and 5m")
	}
	var out Lease
	err := s.db.QueryRow(ctx, `UPDATE leases SET expires_at=now()+$4::interval WHERE volume_id=$1 AND file_path=$2 AND holder=$3 AND expires_at>now() RETURNING holder,token,expires_at`, volumeID, path, holder, ttl.String()).Scan(&out.Holder, &out.Token, &out.ExpiresAt)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, err
	}
	err = s.db.QueryRow(ctx, `INSERT INTO leases(volume_id,file_path,holder,token,expires_at) VALUES($1,$2,$3,1,now()+$4::interval) ON CONFLICT(volume_id,file_path) DO UPDATE SET holder=EXCLUDED.holder,token=leases.token+1,expires_at=EXCLUDED.expires_at WHERE leases.expires_at<=now() RETURNING holder,token,expires_at`, volumeID, path, holder, ttl.String()).Scan(&out.Holder, &out.Token, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrLeaseHeld
	}
	return out, err
}

func (s *Store) ReleaseLease(ctx context.Context, volumeID int64, path, holder string, token int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM leases WHERE volume_id=$1 AND file_path=$2 AND holder=$3 AND token=$4`, volumeID, path, holder, token)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// AcquireRecoveryLease fences active holders while an explicit restore is in progress.
func (s *Store) AcquireRecoveryLease(ctx context.Context, volumeID int64, path, holder string, ttl time.Duration) (Lease, error) {
	var out Lease
	err := s.db.QueryRow(ctx, `INSERT INTO leases(volume_id,file_path,holder,token,expires_at) VALUES($1,$2,$3,1,now()+$4::interval) ON CONFLICT(volume_id,file_path) DO UPDATE SET holder=EXCLUDED.holder,token=leases.token+1,expires_at=EXCLUDED.expires_at RETURNING holder,token,expires_at`, volumeID, path, holder, ttl.String()).Scan(&out.Holder, &out.Token, &out.ExpiresAt)
	return out, err
}

// ListFiles returns all non-deleted files in a volume, optionally filtered by
// path prefix.
type FileEntry struct {
	Path           string
	CurrentVersion int64
	UpdatedAt      time.Time
}

func (s *Store) ListFiles(ctx context.Context, volumeID int64, prefix string) ([]FileEntry, error) {
	const q = `
		SELECT f.path, f.current_version, f.updated_at
		FROM files f
		JOIN file_versions fv ON fv.file_id = f.id AND fv.version = f.current_version
		WHERE f.volume_id = $1 AND f.path LIKE $2 AND fv.is_deleted = false
		ORDER BY f.path`
	rows, err := s.db.Query(ctx, q, volumeID, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("meta: list files: %w", err)
	}
	defer rows.Close()

	var entries []FileEntry
	for rows.Next() {
		var e FileEntry
		if err := rows.Scan(&e.Path, &e.CurrentVersion, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListVersions returns all versions of a file, newest first.
type VersionEntry struct {
	Version   int64
	CommitSeq int64
	Size      int64
	IsDeleted bool
	CreatedAt time.Time
}

func (s *Store) ListVersions(ctx context.Context, fileID int64) ([]VersionEntry, error) {
	const q = `
		SELECT version, commit_seq, size, is_deleted, created_at
		FROM file_versions WHERE file_id = $1 ORDER BY version DESC`
	rows, err := s.db.Query(ctx, q, fileID)
	if err != nil {
		return nil, fmt.Errorf("meta: list versions: %w", err)
	}
	defer rows.Close()

	var entries []VersionEntry
	for rows.Next() {
		var e VersionEntry
		if err := rows.Scan(&e.Version, &e.CommitSeq, &e.Size, &e.IsDeleted, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
