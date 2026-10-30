package chunker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// dummyID returns a SHA-256 of the data; good enough for chunker unit tests
// where we don't care about HMAC keys.
func dummyID(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestFixed_EmptyInput(t *testing.T) {
	f := NewFixed(DefaultChunkSize)
	chunks, err := f.Split(bytes.NewReader(nil), dummyID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks, got %d", len(chunks))
	}
}

func TestFixed_ExactOneChunk(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	f := NewFixed(100)
	chunks, err := f.Split(bytes.NewReader(data), dummyID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0].Data, data) {
		t.Fatal("chunk data does not match input")
	}
}

func TestFixed_MultipleChunks(t *testing.T) {
	// 250 bytes with chunk size 100 → chunks of 100, 100, 50.
	data := make([]byte, 250)
	for i := range data {
		data[i] = byte(i)
	}
	f := NewFixed(100)
	chunks, err := f.Split(bytes.NewReader(data), dummyID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	// Reassemble and verify round-trip.
	var got []byte
	for _, c := range chunks {
		got = append(got, c.Data...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reassembled data does not match original")
	}
}

func TestFixed_DedupSameChunk(t *testing.T) {
	// Two identical blocks should produce two chunks with the same ID.
	block := bytes.Repeat([]byte("a"), 100)
	data := append(block, block...)
	f := NewFixed(100)
	chunks, err := f.Split(bytes.NewReader(data), dummyID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0].ID != chunks[1].ID {
		t.Fatal("identical blocks must produce identical chunk IDs")
	}
}
