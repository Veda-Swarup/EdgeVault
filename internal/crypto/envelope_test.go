package crypto

import (
	"bytes"
	"testing"
)

func TestChunkEnvelopeRoundTripAndTamperDetection(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	payload := []byte("compressed bytes")
	stored, err := EncryptChunk(key, payload, CompressionZstd)
	if err != nil {
		t.Fatal(err)
	}
	got, codec, err := DecryptChunk(key, stored)
	if err != nil {
		t.Fatal(err)
	}
	if codec != CompressionZstd || !bytes.Equal(got, payload) {
		t.Fatalf("got codec %d payload %q", codec, got)
	}
	stored[len(stored)-1] ^= 1
	if _, _, err = DecryptChunk(key, stored); err == nil {
		t.Fatal("tampered ciphertext unexpectedly authenticated")
	}
}
