package blob

import (
	"context"
	"io"
	"time"
)

// Blob is the interface the edge node uses to talk to object storage.
// The real implementation calls S3; tests inject a fake.
// Keeping this thin means we can swap backends (MinIO ↔ AWS S3) by changing
// one constructor without touching the rest of the code.
type Blob interface {
	// Put stores the data at key.  Callers supply the full S3 key.
	Put(ctx context.Context, bucket, key string, data io.Reader, size int64) error

	// Get retrieves the object at key and returns a reader.
	// The caller must close the returned ReadCloser.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// Exists reports whether an object exists without downloading it.
	Exists(ctx context.Context, bucket, key string) (bool, error)
}

// ProtectedBlob stores an object under a time-bounded Object Lock retention.
type ProtectedBlob interface {
	PutProtected(ctx context.Context, bucket, key string, data io.Reader, size int64, retainUntil time.Time) error
}

// Deleter is intentionally separate from Blob so edge nodes cannot delete chunks.
type Deleter interface {
	Delete(ctx context.Context, bucket, key string) error
}

// S3Key returns the canonical S3 key for a chunk in a volume.
// Layout: chunks/<volume>/<first2hex>/<chunk_id>
// The two-character prefix prevents key-space hot spots on S3, which
// partitions by prefix; distributing across 256 prefixes spreads load.
func S3Key(volume, chunkID string) string {
	if len(chunkID) < 2 {
		return "chunks/" + volume + "/00/" + chunkID
	}
	return "chunks/" + volume + "/" + chunkID[:2] + "/" + chunkID
}
