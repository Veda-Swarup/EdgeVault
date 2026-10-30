package chunker

import "io"

// Chunk is a single contiguous slice of plaintext content identified by its
// HMAC-SHA256 chunk ID (hex-encoded).
type Chunk struct {
	ID   string // HMAC-SHA256(volumeKey, plaintext) — hex-encoded
	Data []byte // plaintext bytes
}

// Chunker splits an io.Reader into Chunks.
// The interface is defined here so Phase 1 uses fixed 4 MiB and Phase 2
// can swap in FastCDC without touching the call sites.
type Chunker interface {
	// Split reads all of r and returns the ordered list of chunks.
	Split(r io.Reader, chunkID func([]byte) string) ([]Chunk, error)
}
