package crypto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ChunkIDFunc returns a function that computes HMAC-SHA256(key, data) as a
// hex string.  This is used as the Chunker.chunkID argument.
//
// Why HMAC instead of plain SHA-256?
//   - Plain SHA-256 is a global content-address: anyone who knows a file's
//     SHA-256 hash can test whether that exact file is stored in *any* volume
//     by looking for the key in S3 (a "confirmation of a file" oracle attack).
//   - HMAC-SHA256(volumeKey, chunk) binds the ID to the volume's secret key,
//     so dedup still works *within* a volume but the ID reveals nothing to
//     outsiders.
func ChunkIDFunc(key []byte) func([]byte) string {
	return func(data []byte) string {
		mac := hmac.New(sha256.New, key)
		mac.Write(data)
		return hex.EncodeToString(mac.Sum(nil))
	}
}

// ManifestHash returns SHA-256 of the concatenated ordered chunk IDs.
// This is stored in file_versions.manifest_hash and used for the no-change
// dedup check: if the hash matches the current version, we skip the commit.
func ManifestHash(chunkIDs []string) []byte {
	h := sha256.New()
	for _, id := range chunkIDs {
		h.Write([]byte(id))
		h.Write([]byte{0}) // null-byte separator to prevent length-extension
	}
	return h.Sum(nil)
}

// ParseHMACKey decodes a hex-encoded 32-byte HMAC key.
// Returns an error if the key is absent or not exactly 32 bytes.
func ParseHMACKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		return nil, fmt.Errorf("crypto: HMAC key is empty")
	}
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("crypto: decode HMAC key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: HMAC key must be 32 bytes, got %d", len(key))
	}
	return key, nil
}

// EqualManifestHash is a constant-time comparison of two manifest hashes.
func EqualManifestHash(a, b []byte) bool {
	return bytes.Equal(a, b) // SHA-256 output is fixed-length; timing-safe
}
