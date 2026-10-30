package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vedaswarup/edgevault/internal/blob"
	"github.com/vedaswarup/edgevault/internal/cache"
	"github.com/vedaswarup/edgevault/internal/chunker"
	"github.com/vedaswarup/edgevault/internal/crypto"
	"github.com/vedaswarup/edgevault/internal/journal"
	"github.com/vedaswarup/edgevault/internal/kms"
	"github.com/vedaswarup/edgevault/internal/meta"
	"github.com/vedaswarup/edgevault/internal/metrics"
	"github.com/vedaswarup/edgevault/internal/uploader"
)

// Handler holds the dependencies shared across all API endpoints.
// All fields are safe for concurrent use.
type Handler struct {
	store          *meta.Store
	blob           blob.Blob
	chunker        chunker.Chunker
	hmacKey        []byte // global key for Phase 1; replaced per-volume in Phase 3
	bucket         string // chunks bucket name
	log            *slog.Logger
	cache          *cache.Cache
	journal        *journal.Journal
	uploader       *uploader.Uploader
	metrics        *metrics.Metrics
	kms            kms.KMS
	kmsKeyID       string
	keyMu          sync.RWMutex
	dataKeys       map[int64][]byte
	snapshotBucket string
	quarantineMu   sync.RWMutex
	quarantined    map[string]map[int64]bool
	adminToken     string
}

// NewHandlerWithRuntime enables the Phase 2 durable cache and uploader.
func NewHandlerWithRuntime(store *meta.Store, bl blob.Blob, chkr chunker.Chunker, hmacKey []byte, bucket string, log *slog.Logger, c *cache.Cache, j *journal.Journal, u *uploader.Uploader, m *metrics.Metrics) *Handler {
	h := NewHandler(store, bl, chkr, hmacKey, bucket, log)
	h.cache = c
	h.journal = j
	h.uploader = u
	h.metrics = m
	return h
}

func NewHandlerWithKMS(store *meta.Store, bl blob.Blob, chkr chunker.Chunker, hmacKey []byte, bucket string, log *slog.Logger, keyManager kms.KMS, kmsKeyID string, restoredKeys map[int64][]byte) *Handler {
	h := NewHandler(store, bl, chkr, hmacKey, bucket, log)
	h.kms = keyManager
	h.kmsKeyID = kmsKeyID
	h.dataKeys = restoredKeys
	if h.dataKeys == nil {
		h.dataKeys = make(map[int64][]byte)
	}
	h.snapshotBucket = bucket
	h.quarantined = make(map[string]map[int64]bool)
	return h
}

func NewHandlerWithFullRuntime(store *meta.Store, bl blob.Blob, chkr chunker.Chunker, hmacKey []byte, bucket string, log *slog.Logger, c *cache.Cache, j *journal.Journal, u *uploader.Uploader, m *metrics.Metrics, keyManager kms.KMS, kmsKeyID string, restoredKeys map[int64][]byte) *Handler {
	h := NewHandlerWithRuntime(store, bl, chkr, hmacKey, bucket, log, c, j, u, m)
	h.kms = keyManager
	h.kmsKeyID = kmsKeyID
	h.dataKeys = restoredKeys
	if h.dataKeys == nil {
		h.dataKeys = make(map[int64][]byte)
	}
	h.snapshotBucket = bucket
	h.quarantined = make(map[string]map[int64]bool)
	return h
}

func (h *Handler) SetSnapshotBucket(bucket string) {
	if bucket != "" {
		h.snapshotBucket = bucket
	}
}

func (h *Handler) SetAdminToken(token string) { h.adminToken = token }

func (h *Handler) isQuarantined(volumeID int64, client string) bool {
	h.quarantineMu.RLock()
	defer h.quarantineMu.RUnlock()
	return h.quarantined[client][volumeID]
}
func (h *Handler) quarantine(volumeID int64, client string) {
	h.quarantineMu.Lock()
	defer h.quarantineMu.Unlock()
	if h.quarantined[client] == nil {
		h.quarantined[client] = make(map[int64]bool)
	}
	h.quarantined[client][volumeID] = true
}
func (h *Handler) clearQuarantine(volumeID int64) {
	h.quarantineMu.Lock()
	defer h.quarantineMu.Unlock()
	for client, volumes := range h.quarantined {
		delete(volumes, volumeID)
		if len(volumes) == 0 {
			delete(h.quarantined, client)
		}
	}
}

func (h *Handler) protectSnapshot(ctx context.Context, vol meta.Volume, seq int64, reason string) (meta.Snapshot, error) {
	protected, ok := h.blob.(blob.ProtectedBlob)
	if !ok {
		return meta.Snapshot{}, errors.New("blob backend does not support object lock")
	}
	if h.snapshotBucket == "" {
		return meta.Snapshot{}, errors.New("protected snapshot bucket is not configured")
	}
	snap, err := h.store.CreateSnapshot(ctx, vol.ID, seq, true, reason)
	if err != nil {
		return meta.Snapshot{}, err
	}
	entries, err := h.store.StateAt(ctx, vol.ID, seq, "")
	if err != nil {
		_ = h.store.DeleteSnapshot(ctx, vol.ID, snap.ID)
		return meta.Snapshot{}, err
	}
	state := make([]meta.StateEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDeleted {
			state = append(state, entry)
		}
	}
	payload, err := json.Marshal(struct {
		Volume    string            `json:"volume"`
		CommitSeq int64             `json:"commit_seq"`
		Files     []meta.StateEntry `json:"files"`
	}{vol.Name, seq, state})
	if err != nil {
		_ = h.store.DeleteSnapshot(ctx, vol.ID, snap.ID)
		return meta.Snapshot{}, err
	}
	key := fmt.Sprintf("protected/%s/%d.json", vol.Name, snap.ID)
	if err = protected.PutProtected(ctx, h.snapshotBucket, key, bytes.NewReader(payload), int64(len(payload)), time.Now().Add(7*24*time.Hour)); err != nil {
		_ = h.store.DeleteSnapshot(ctx, vol.ID, snap.ID)
		return meta.Snapshot{}, err
	}
	if err = h.store.SetSnapshotS3Key(ctx, vol.ID, snap.ID, key); err != nil {
		return meta.Snapshot{}, err
	}
	snap.S3Key = key
	return snap, nil
}

func (h *Handler) volumeKey(ctx context.Context, vol meta.Volume) ([]byte, bool, error) {
	if vol.KeyVersion < 2 {
		return h.hmacKey, false, nil
	}
	h.keyMu.RLock()
	key := h.dataKeys[vol.ID]
	h.keyMu.RUnlock()
	if len(key) != 32 {
		if h.kms == nil {
			return nil, true, fmt.Errorf("no KMS available to unwrap data key for volume %q", vol.Name)
		}
		var err error
		key, err = h.kms.Decrypt(ctx, vol.WrappedDataKey)
		if err != nil {
			return nil, true, fmt.Errorf("unwrap data key for volume %q: %w", vol.Name, err)
		}
		if len(key) != 32 {
			return nil, true, fmt.Errorf("unwrapped data key for volume %q has invalid length %d", vol.Name, len(key))
		}
		h.keyMu.Lock()
		if existing := h.dataKeys[vol.ID]; len(existing) == 32 {
			key = existing
		} else {
			h.dataKeys[vol.ID] = key
		}
		h.keyMu.Unlock()
	}
	return key, true, nil
}

// NewHandler constructs an API handler.
func NewHandler(
	store *meta.Store,
	bl blob.Blob,
	chkr chunker.Chunker,
	hmacKey []byte,
	bucket string,
	log *slog.Logger,
) *Handler {
	return &Handler{
		store:   store,
		blob:    bl,
		chunker: chkr,
		hmacKey: hmacKey,
		bucket:  bucket,
		log:     log,
	}
}

// --- HTTP helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// volPath extracts {vol} and {path} from a URL like /v1/volumes/{vol}/files/{path...}
// where path may contain slashes.
func splitVolPath(urlPath, prefix string) (vol, filePath string, ok bool) {
	// prefix = "/v1/volumes/"
	rest := strings.TrimPrefix(urlPath, prefix)
	// rest = "<vol>/files/<path>" or "<vol>/files"
	idx := strings.Index(rest, "/files")
	if idx < 0 {
		return "", "", false
	}
	vol = rest[:idx]
	after := rest[idx+len("/files"):]
	// after = "" | "/" | "/<path>"
	filePath = strings.TrimPrefix(after, "/")
	return vol, filePath, true
}

// resolveVolume looks up the volume by name or returns a 404.
func (h *Handler) resolveVolume(ctx context.Context, w http.ResponseWriter, volName string) (meta.Volume, bool) {
	vol, err := h.store.GetVolumeByName(ctx, volName)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "volume not found: "+volName)
		return meta.Volume{}, false
	}
	if err != nil {
		h.log.Error("resolve volume", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return meta.Volume{}, false
	}
	return vol, true
}

// --- PUT /v1/volumes/{vol}/files/{path} ---

type putResponse struct {
	Version   int64  `json:"version"`
	CommitSeq int64  `json:"commit_seq"`
	Status    string `json:"status"` // "created" | "unchanged"
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	volName, filePath, ok := splitVolPath(r.URL.Path, "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	vol, ok := h.resolveVolume(ctx, w, volName)
	if !ok {
		return
	}
	client := r.Header.Get("X-Edge-Node")
	if client == "" {
		client = "unknown"
	}
	if vol.AlertOpen || h.isQuarantined(vol.ID, client) {
		writeError(w, http.StatusLocked, "writes paused after ransomware alert; restore the clean snapshot")
		return
	}
	leaseToken, err := leaseTokenFromRequest(r)
	if err != nil {
		writeError(w, http.StatusLocked, "valid X-Edge-Lease-Token is required")
		return
	}
	var previousEntropy *float32
	priorFile, priorErr := h.store.GetFile(ctx, vol.ID, filePath)
	if priorErr == nil && priorFile.CurrentVersion > 0 {
		if priorVersion, e := h.store.GetFileVersion(ctx, priorFile.ID, priorFile.CurrentVersion); e == nil {
			previousEntropy = priorVersion.AvgEntropy
		}
	} else if priorErr != nil && !errors.Is(priorErr, meta.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Parse If-Match header: the version the client last saw.
	// 0 means "I think this is a new file".
	var expectedVersion int64
	if im := r.Header.Get("If-Match"); im != "" {
		v, err := strconv.ParseInt(im, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "If-Match must be an integer version")
			return
		}
		expectedVersion = v
	}

	// New volumes bind content IDs to their wrapped per-volume key. Legacy
	// volumes created before envelope encryption retain their original key path.
	volumeKey, encrypted, err := h.volumeKey(ctx, vol)
	if err != nil {
		h.log.Error("volume key", "err", err)
		writeError(w, http.StatusInternalServerError, "volume key unavailable")
		return
	}
	// 1. Read and chunk the body.
	chunkIDFn := crypto.ChunkIDFunc(volumeKey)
	chunks, err := h.chunker.Split(r.Body, chunkIDFn)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var entropySum float64
	for _, c := range chunks {
		entropySum += crypto.Entropy(c.Data)
	}
	avgEntropy := float32(0)
	if len(chunks) > 0 {
		avgEntropy = float32(entropySum / float64(len(chunks)))
	}

	// 2. Check manifest hash for no-change dedup.
	chunkIDs := make([]string, len(chunks))
	for i, c := range chunks {
		chunkIDs[i] = c.ID
	}
	manifestHash := crypto.ManifestHash(chunkIDs)

	currentHash, err := h.store.GetCurrentManifestHash(ctx, vol.ID, filePath)
	if err != nil {
		h.log.Error("get manifest hash", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if crypto.EqualManifestHash(currentHash, manifestHash) {
		// Identical content — return the current version without writing.
		file, err := h.store.GetFile(ctx, vol.ID, filePath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, putResponse{
			Version:   file.CurrentVersion,
			CommitSeq: 0, // no new commit_seq was allocated
			Status:    "unchanged",
		})
		return
	}

	// 3. Upload new chunks to S3 (deduplicate within the volume).
	var vcEntries []meta.VersionChunk
	var totalSize int64
	var offset int64
	uploadChunk := func(c chunker.Chunk, s3Key string) error {
		payload, codec, err := crypto.Compress(c.Data)
		if err != nil {
			return err
		}
		stored := payload
		if encrypted {
			stored, err = crypto.EncryptChunk(volumeKey, payload, codec)
			if err != nil {
				return err
			}
		}
		if h.uploader != nil {
			err = h.uploader.Submit(ctx, h.bucket, s3Key, stored)
		} else {
			err = h.blob.Put(ctx, h.bucket, s3Key, bytes.NewReader(stored), int64(len(stored)))
		}
		if err != nil {
			return err
		}
		return h.store.UpsertChunk(ctx, meta.ChunkMeta{VolumeID: vol.ID, ChunkID: c.ID, Size: len(c.Data), StoredSize: len(stored), Compression: codec, S3Key: s3Key})
	}

	for i, c := range chunks {
		if h.metrics != nil {
			h.metrics.TotalChunks.Add(1)
		}
		s3Key := blob.S3Key(volName, c.ID)

		// Check if the chunk already exists for this volume.
		_, getErr := h.store.GetChunk(ctx, vol.ID, c.ID)
		if errors.Is(getErr, meta.ErrNotFound) {
			if err := uploadChunk(c, s3Key); err != nil {
				h.log.Error("put chunk", "chunk_id", c.ID, "err", err)
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("upload chunk %s: %v", c.ID, err))
				return
			}
		} else if getErr != nil {
			h.log.Error("get chunk", "err", getErr)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		} else {
			touched, touchErr := h.store.TouchChunk(ctx, vol.ID, c.ID)
			if touchErr != nil {
				writeError(w, http.StatusInternalServerError, "update chunk use time")
				return
			}
			if !touched {
				if err := uploadChunk(c, s3Key); err != nil {
					writeError(w, http.StatusInternalServerError, "re-upload collected chunk")
					return
				}
			} else if h.metrics != nil {
				h.metrics.DedupChunks.Add(1)
			}
		}

		vcEntries = append(vcEntries, meta.VersionChunk{
			Idx:     i,
			ChunkID: c.ID,
			Size:    len(c.Data),
			Offset:  offset,
		})
		offset += int64(len(c.Data))
		totalSize += int64(len(c.Data))
	}

	// 4. Commit the version.
	result, err := h.store.Commit(ctx, meta.CommitInput{
		VolumeID:        vol.ID,
		FilePath:        filePath,
		ExpectedVersion: expectedVersion,
		Size:            totalSize,
		ManifestHash:    manifestHash,
		CreatedBy:       r.Header.Get("X-Edge-Node"),
		Chunks:          vcEntries,
		AvgEntropy:      &avgEntropy,
		LeaseHolder:     client,
		LeaseToken:      leaseToken,
	})
	if errors.Is(err, meta.ErrVolumeAlertOpen) {
		writeError(w, http.StatusLocked, "writes paused after ransomware alert; restore the clean snapshot")
		return
	}
	if errors.Is(err, meta.ErrConflict) || errors.Is(err, meta.ErrLeaseLost) {
		if h.metrics != nil {
			h.metrics.CommitConflicts.Add(1)
			if errors.Is(err, meta.ErrLeaseLost) {
				h.metrics.LeaseConflicts.Add(1)
			}
		}
		conflictPath, copyErr := h.saveConflictCopy(ctx, vol.ID, filePath, client, totalSize, manifestHash, vcEntries, &avgEntropy)
		if copyErr != nil {
			h.log.Error("save rejected write as conflict copy", "err", copyErr)
		} else {
			w.Header().Set("X-EdgeVault-Conflict-Path", conflictPath)
		}
		writeError(w, http.StatusConflict, "version conflict; re-fetch and retry")
		return
	}
	if err != nil {
		h.log.Error("commit", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	decision, detectorErr := h.store.ObserveWriteSignal(ctx, vol.ID, filePath, client, result.CommitSeq-1, previousEntropy, &avgEntropy)
	if detectorErr != nil {
		h.log.Error("record ransomware signal", "err", detectorErr)
	} else {
		if h.metrics != nil {
			h.metrics.RansomwareScore.Store(int64(decision.Score * 1000))
		}
		if decision.Alert {
			snapshot, snapErr := h.protectSnapshot(ctx, vol, decision.BaselineSeq, "ransomware")
			if snapErr != nil {
				h.log.Error("protect ransomware snapshot", "err", snapErr)
			} else if err := h.store.OpenAlert(ctx, vol.ID, client, snapshot.ID, decision.Score); err != nil {
				h.log.Error("record protected ransomware snapshot", "err", err)
			}
			h.quarantine(vol.ID, client)
			if h.metrics != nil {
				h.metrics.RansomwareAlerts.Add(1)
			}
		}
	}

	writeJSON(w, http.StatusCreated, putResponse{
		Version:   result.Version,
		CommitSeq: result.CommitSeq,
		Status:    "created",
	})
}

// --- GET /v1/volumes/{vol}/files/{path}?version=N ---

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	volName, filePath, ok := splitVolPath(r.URL.Path, "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	vol, ok := h.resolveVolume(ctx, w, volName)
	if !ok {
		return
	}

	file, err := h.store.GetFile(ctx, vol.ID, filePath)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	volumeKey, encrypted, err := h.volumeKey(ctx, vol)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "volume key unavailable")
		return
	}

	// Determine which version to serve.
	targetVersion := file.CurrentVersion
	if vStr := r.URL.Query().Get("version"); vStr != "" {
		v, err := strconv.ParseInt(vStr, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "version must be an integer")
			return
		}
		targetVersion = v
	}

	fv, err := h.store.GetFileVersion(ctx, file.ID, targetVersion)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if fv.IsDeleted {
		writeError(w, http.StatusGone, "file has been deleted")
		return
	}

	chunks, err := h.store.GetVersionChunks(ctx, file.ID, targetVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Verify and assemble before writing headers so an authentication failure
	// cannot be mistaken for a successful partial download.
	var fileData bytes.Buffer
	chunkIDFn := crypto.ChunkIDFunc(volumeKey)
	// Fetch stored bytes through the cache, then decrypt and decompress to plaintext.
	for _, c := range chunks {
		cm, cerr := h.store.GetChunk(ctx, vol.ID, c.ChunkID)
		if cerr != nil {
			h.log.Error("get chunk meta", "err", cerr)
			return
		}
		var stored []byte
		var err error
		if h.uploader != nil && h.cache != nil && h.metrics != nil {
			stored, _, err = uploader.Fetch(ctx, h.blob, h.cache, h.metrics, h.bucket, cm.S3Key)
		} else {
			var rc io.ReadCloser
			rc, err = h.blob.Get(ctx, h.bucket, cm.S3Key)
			if err == nil {
				stored, err = io.ReadAll(rc)
				_ = rc.Close()
			}
		}
		if err != nil {
			h.log.Error("fetch chunk", "s3_key", cm.S3Key, "err", err)
			return
		}
		compressed := stored
		codec := cm.Compression
		if encrypted {
			compressed, codec, err = crypto.DecryptChunk(volumeKey, stored)
			if err != nil {
				h.log.Error("decrypt chunk", "chunk_id", c.ChunkID, "err", err)
				writeError(w, http.StatusInternalServerError, "chunk authentication failed")
				return
			}
		}
		if codec != cm.Compression {
			writeError(w, http.StatusInternalServerError, "chunk compression metadata mismatch")
			return
		}
		plain, err := crypto.Decompress(compressed, codec)
		if err != nil {
			h.log.Error("decompress chunk", "chunk_id", c.ChunkID, "err", err)
			return
		}
		if chunkIDFn(plain) != c.ChunkID {
			writeError(w, http.StatusInternalServerError, "chunk integrity check failed")
			return
		}
		_, _ = fileData.Write(plain)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-EdgeVault-Version", strconv.FormatInt(targetVersion, 10))
	w.Header().Set("X-EdgeVault-Size", strconv.FormatInt(fv.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(fileData.Bytes())
}

// --- GET /v1/volumes/{vol}/files?prefix= ---

type listResponse struct {
	Files []fileEntry `json:"files"`
}

type fileEntry struct {
	Path           string `json:"path"`
	CurrentVersion int64  `json:"current_version"`
}

func (h *Handler) handleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	volName := strings.TrimPrefix(r.URL.Path, "/v1/volumes/")
	volName = strings.TrimSuffix(volName, "/files")

	vol, ok := h.resolveVolume(ctx, w, volName)
	if !ok {
		return
	}

	prefix := r.URL.Query().Get("prefix")
	entries, err := h.store.ListFiles(ctx, vol.ID, prefix)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := listResponse{}
	for _, e := range entries {
		resp.Files = append(resp.Files, fileEntry{Path: e.Path, CurrentVersion: e.CurrentVersion})
	}
	if resp.Files == nil {
		resp.Files = []fileEntry{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- GET /v1/volumes/{vol}/files/{path}/versions ---

type versionsResponse struct {
	Versions []versionEntry `json:"versions"`
}

type versionEntry struct {
	Version   int64  `json:"version"`
	CommitSeq int64  `json:"commit_seq"`
	Size      int64  `json:"size"`
	IsDeleted bool   `json:"is_deleted"`
	CreatedAt string `json:"created_at"`
}

func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// URL: /v1/volumes/{vol}/files/{path}/versions
	path := r.URL.Path
	// strip /versions suffix
	path = strings.TrimSuffix(path, "/versions")
	volName, filePath, ok := splitVolPath(path, "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	vol, ok := h.resolveVolume(ctx, w, volName)
	if !ok {
		return
	}

	file, err := h.store.GetFile(ctx, vol.ID, filePath)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	entries, err := h.store.ListVersions(ctx, file.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := versionsResponse{}
	for _, e := range entries {
		resp.Versions = append(resp.Versions, versionEntry{
			Version:   e.Version,
			CommitSeq: e.CommitSeq,
			Size:      e.Size,
			IsDeleted: e.IsDeleted,
			CreatedAt: e.CreatedAt.String(),
		})
	}
	if resp.Versions == nil {
		resp.Versions = []versionEntry{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- DELETE /v1/volumes/{vol}/files/{path} ---

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	volName, filePath, ok := splitVolPath(r.URL.Path, "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	vol, ok := h.resolveVolume(ctx, w, volName)
	if !ok {
		return
	}
	client := r.Header.Get("X-Edge-Node")
	if client == "" {
		client = "unknown"
	}
	if vol.AlertOpen || h.isQuarantined(vol.ID, client) {
		writeError(w, http.StatusLocked, "writes paused after ransomware alert; restore the clean snapshot")
		return
	}
	leaseToken, err := leaseTokenFromRequest(r)
	if err != nil {
		writeError(w, http.StatusLocked, "valid X-Edge-Lease-Token is required")
		return
	}

	result, err := h.store.CommitTombstone(ctx, vol.ID, filePath, client, client, leaseToken, false)
	if errors.Is(err, meta.ErrNotFound) {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		if errors.Is(err, meta.ErrVolumeAlertOpen) {
			writeError(w, http.StatusLocked, "writes paused after ransomware alert; restore the clean snapshot")
			return
		}
		if errors.Is(err, meta.ErrLeaseLost) {
			writeError(w, http.StatusLocked, "lease expired or fencing token is stale")
			return
		}
		h.log.Error("tombstone", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":    result.Version,
		"commit_seq": result.CommitSeq,
		"status":     "deleted",
	})
}

func leaseTokenFromRequest(r *http.Request) (int64, error) {
	value := r.Header.Get("X-Edge-Lease-Token")
	if value == "" {
		return 0, meta.ErrLeaseLost
	}
	token, err := strconv.ParseInt(value, 10, 64)
	if err != nil || token < 1 {
		return 0, meta.ErrLeaseLost
	}
	return token, nil
}

func (h *Handler) handleAcquireLease(w http.ResponseWriter, r *http.Request) {
	volName, filePath, ok := splitVolPath(strings.TrimSuffix(r.URL.Path, "/lease"), "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid file path")
		return
	}
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	holder := r.Header.Get("X-Edge-Node")
	if holder == "" {
		writeError(w, http.StatusBadRequest, "X-Edge-Node is required")
		return
	}
	var req struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid lease body")
			return
		}
	}
	ttl := 30 * time.Second
	if req.TTLSeconds != 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	lease, err := h.store.AcquireLease(r.Context(), vol.ID, filePath, holder, ttl)
	if errors.Is(err, meta.ErrLeaseHeld) {
		if h.metrics != nil {
			h.metrics.LeaseConflicts.Add(1)
		}
		writeError(w, http.StatusLocked, "lease held by another edge")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"holder": lease.Holder, "token": lease.Token, "expires_at": lease.ExpiresAt})
}

func (h *Handler) handleReleaseLease(w http.ResponseWriter, r *http.Request) {
	volName, filePath, ok := splitVolPath(strings.TrimSuffix(r.URL.Path, "/lease"), "/v1/volumes/")
	if !ok || filePath == "" {
		writeError(w, http.StatusBadRequest, "invalid file path")
		return
	}
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	holder := r.Header.Get("X-Edge-Node")
	token, err := leaseTokenFromRequest(r)
	if holder == "" || err != nil {
		writeError(w, http.StatusBadRequest, "holder and lease token are required")
		return
	}
	err = h.store.ReleaseLease(r.Context(), vol.ID, filePath, holder, token)
	if errors.Is(err, meta.ErrLeaseLost) {
		writeError(w, http.StatusConflict, "lease is no longer held by this edge")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) saveConflictCopy(ctx context.Context, volumeID int64, original, holder string, size int64, manifest []byte, chunks []meta.VersionChunk, entropy *float32) (string, error) {
	label := strings.NewReplacer("/", "_", "\\", "_", " ", "_").Replace(holder)
	if label == "" {
		label = "unknown"
	}
	ext := path.Ext(original)
	stem := strings.TrimSuffix(original, ext)
	for i := 1; i <= 20; i++ {
		suffix := label
		if i > 1 {
			suffix = fmt.Sprintf("%s-%d", label, i)
		}
		candidate := fmt.Sprintf("%s (conflict %s)%s", stem, suffix, ext)
		_, err := h.store.Commit(ctx, meta.CommitInput{VolumeID: volumeID, FilePath: candidate, ExpectedVersion: 0, Size: size, ManifestHash: manifest, CreatedBy: holder, Chunks: chunks, AvgEntropy: entropy, AllowConflictCopy: true})
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, meta.ErrConflict) {
			return "", err
		}
	}
	return "", errors.New("could not allocate unique conflict-copy path")
}

// --- POST /v1/volumes ---

func (h *Handler) handleState(w http.ResponseWriter, r *http.Request) {
	volName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/volumes/"), "/state")
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.URL.Query().Get("as_of_seq"), 10, 64)
	if err != nil || seq < 0 || seq > vol.HeadSeq {
		writeError(w, http.StatusBadRequest, "as_of_seq must be between 0 and the current head_seq")
		return
	}
	entries, err := h.store.StateAt(r.Context(), vol.ID, seq, r.URL.Query().Get("prefix"))
	if err != nil {
		h.log.Error("state query", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	type stateFile struct {
		Path    string `json:"path"`
		Version int64  `json:"version"`
	}
	out := make([]stateFile, 0, len(entries))
	for _, e := range entries {
		if !e.IsDeleted {
			out = append(out, stateFile{e.Path, e.Version})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"as_of_seq": seq, "files": out})
}

func (h *Handler) handleRetention(w http.ResponseWriter, r *http.Request) {
	if h.adminToken == "" {
		writeError(w, http.StatusServiceUnavailable, "admin token is not configured")
		return
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !hmac.Equal([]byte(provided), []byte(h.adminToken)) {
		writeError(w, http.StatusUnauthorized, "admin authorization required")
		return
	}
	volName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/volumes/"), "/retention")
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	var policy struct {
		KeepDays    int `json:"keep_days"`
		MinVersions int `json:"min_versions"`
		FloorDays   int `json:"floor_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		writeError(w, http.StatusBadRequest, "body must include keep_days, min_versions, and floor_days")
		return
	}
	if err := h.store.SetRetentionPolicy(r.Context(), vol.ID, r.Header.Get("X-Edge-Node"), policy.KeepDays, policy.MinVersions, policy.FloorDays); errors.Is(err, meta.ErrRetentionFloor) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if err != nil {
		h.log.Error("set retention policy", "err", err)
		writeError(w, http.StatusInternalServerError, "could not update retention policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"keep_days": policy.KeepDays, "min_versions": policy.MinVersions, "floor_days": policy.FloorDays})
}

func (h *Handler) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	volName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/volumes/"), "/snapshots")
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	var req struct {
		Protected bool   `json:"protected"`
		Reason    string `json:"reason"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid snapshot body")
			return
		}
	}
	if req.Reason == "" {
		req.Reason = "manual"
	}
	var snap meta.Snapshot
	var err error
	if req.Protected {
		snap, err = h.protectSnapshot(r.Context(), vol, vol.HeadSeq, req.Reason)
	} else {
		snap, err = h.store.TakeSnapshot(r.Context(), vol.ID, false, req.Reason)
	}
	if err != nil {
		h.log.Error("create snapshot", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": snap.ID, "commit_seq": snap.CommitSeq, "protected": snap.Protected, "reason": snap.Reason})
}

func (h *Handler) handleRestore(w http.ResponseWriter, r *http.Request) {
	volName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/volumes/"), "/restore")
	vol, ok := h.resolveVolume(r.Context(), w, volName)
	if !ok {
		return
	}
	var req struct {
		AsOfSeq    *int64   `json:"as_of_seq"`
		SnapshotID *int64   `json:"snapshot_id"`
		Paths      []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must include as_of_seq or snapshot_id")
		return
	}
	if (req.AsOfSeq == nil) == (req.SnapshotID == nil) {
		writeError(w, http.StatusBadRequest, "specify exactly one of as_of_seq or snapshot_id")
		return
	}
	seq := int64(0)
	if req.SnapshotID != nil {
		snap, err := h.store.GetSnapshot(r.Context(), vol.ID, *req.SnapshotID)
		if errors.Is(err, meta.ErrNotFound) {
			writeError(w, http.StatusNotFound, "snapshot not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		seq = snap.CommitSeq
	} else {
		seq = *req.AsOfSeq
	}
	if seq < 0 || seq > vol.HeadSeq {
		writeError(w, http.StatusBadRequest, "as_of_seq is outside the volume history")
		return
	}
	state, err := h.store.StateAt(r.Context(), vol.ID, seq, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	selected := map[string]bool{}
	for _, p := range req.Paths {
		selected[p] = true
	}
	filter := len(selected) > 0
	target := map[string]meta.StateEntry{}
	for _, e := range state {
		if !e.IsDeleted && (!filter || selected[e.Path]) {
			target[e.Path] = e
		}
	}
	current, err := h.store.ListFiles(r.Context(), vol.ID, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	changed := 0
	for _, cur := range current {
		if filter && !selected[cur.Path] {
			continue
		}
		if _, exists := target[cur.Path]; !exists {
			lease, leaseErr := h.restoreLease(r.Context(), vol, cur.Path)
			if leaseErr != nil {
				writeError(w, http.StatusLocked, "restore could not fence the current writer")
				return
			}
			_, commitErr := h.store.CommitTombstone(r.Context(), vol.ID, cur.Path, "restore", "restore", lease.Token, true)
			_ = h.store.ReleaseLease(r.Context(), vol.ID, cur.Path, lease.Holder, lease.Token)
			if commitErr != nil && !errors.Is(commitErr, meta.ErrNotFound) {
				h.log.Error("restore tombstone", "path", cur.Path, "err", commitErr)
				writeError(w, http.StatusInternalServerError, "restore failed")
				return
			}
			changed++
		}
	}
	targetPaths := make([]string, 0, len(target))
	for targetPath := range target {
		targetPaths = append(targetPaths, targetPath)
	}
	sort.Strings(targetPaths)
	for _, path := range targetPaths {
		e := target[path]
		currentFile, err := h.store.GetFile(r.Context(), vol.ID, path)
		expected := int64(0)
		if err == nil {
			expected = currentFile.CurrentVersion
		} else if !errors.Is(err, meta.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		ver, err := h.store.GetFileVersion(r.Context(), e.FileID, e.Version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "version metadata unavailable")
			return
		}
		currentHash, err := h.store.GetCurrentManifestHash(r.Context(), vol.ID, path)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if crypto.EqualManifestHash(currentHash, ver.ManifestHash) && expected > 0 {
			continue
		}
		chunks, err := h.store.GetVersionChunks(r.Context(), e.FileID, e.Version)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chunk manifest unavailable")
			return
		}
		lease, leaseErr := h.restoreLease(r.Context(), vol, path)
		if leaseErr != nil {
			writeError(w, http.StatusLocked, "restore could not fence the current writer")
			return
		}
		_, err = h.store.Commit(r.Context(), meta.CommitInput{VolumeID: vol.ID, FilePath: path, ExpectedVersion: expected, Size: ver.Size, ManifestHash: ver.ManifestHash, CreatedBy: "restore", Chunks: chunks, LeaseHolder: "restore", LeaseToken: lease.Token, AllowRecovery: true})
		_ = h.store.ReleaseLease(r.Context(), vol.ID, path, lease.Holder, lease.Token)
		if err != nil {
			if errors.Is(err, meta.ErrConflict) {
				writeError(w, http.StatusConflict, "restore conflict; retry from current state")
			} else {
				h.log.Error("restore commit", "path", path, "err", err)
				writeError(w, http.StatusInternalServerError, "restore failed")
			}
			return
		}
		changed++
	}
	if vol.AlertOpen {
		if err := h.store.CloseAlert(r.Context(), vol.ID, "restore"); err != nil {
			h.log.Error("close ransomware alert", "err", err)
		} else {
			h.clearQuarantine(vol.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "restored", "as_of_seq": seq, "files_changed": changed})
}

func (h *Handler) restoreLease(ctx context.Context, vol meta.Volume, filePath string) (meta.Lease, error) {
	return h.store.AcquireRecoveryLease(ctx, vol.ID, filePath, "restore", 30*time.Second)
}

func (h *Handler) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "body must be {\"name\":\"...\"}")
		return
	}

	if h.kms == nil {
		writeError(w, http.StatusServiceUnavailable, "KMS is not configured")
		return
	}
	keyID := h.kmsKeyID
	if keyID == "" {
		keyID = "local-fake"
	}
	dataKey, wrappedKey, err := h.kms.GenerateDataKey(ctx, keyID)
	if err != nil {
		h.log.Error("generate volume key", "err", err)
		writeError(w, http.StatusInternalServerError, "could not generate volume key")
		return
	}
	vol, err := h.store.CreateVolume(ctx, req.Name, keyID, wrappedKey)
	if err != nil {
		h.log.Error("create volume", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.keyMu.Lock()
	h.dataKeys[vol.ID] = dataKey
	h.keyMu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":   vol.ID,
		"name": vol.Name,
	})
}
