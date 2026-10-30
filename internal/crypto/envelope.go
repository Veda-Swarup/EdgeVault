package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	ChunkFormatVersion byte = 1
	chunkHeaderSize         = 1 + 1 + 12
)

var ErrInvalidChunk = errors.New("invalid encrypted chunk")

// EncryptChunk stores [format version][compression][12-byte nonce][GCM ciphertext+tag].
func EncryptChunk(key, compressed []byte, compression int16) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("AES-256 key must be 32 bytes, got %d", len(key))
	}
	if compression < 0 || compression > 255 {
		return nil, fmt.Errorf("compression id %d does not fit in header", compression)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := make([]byte, chunkHeaderSize, chunkHeaderSize+len(compressed)+aead.Overhead())
	out[0] = ChunkFormatVersion
	out[1] = byte(compression)
	copy(out[2:chunkHeaderSize], nonce)
	return aead.Seal(out, nonce, compressed, out[:2]), nil
}

// DecryptChunk authenticates the payload before exposing its compressed bytes.
func DecryptChunk(key, stored []byte) ([]byte, int16, error) {
	if len(key) != 32 || len(stored) < chunkHeaderSize+16 || stored[0] != ChunkFormatVersion {
		return nil, 0, ErrInvalidChunk
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, 0, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, 0, err
	}
	nonce := stored[2:chunkHeaderSize]
	plain, err := aead.Open(nil, nonce, stored[chunkHeaderSize:], stored[:2])
	if err != nil {
		return nil, 0, fmt.Errorf("authenticate encrypted chunk: %w", err)
	}
	return plain, int16(stored[1]), nil
}
