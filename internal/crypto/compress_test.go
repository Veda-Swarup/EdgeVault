package crypto

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestCompressRoundTripAndHighEntropySkip(t *testing.T) {
	text := bytes.Repeat([]byte("edgevault storage example\n"), 2000)
	stored, codec, err := Compress(text)
	if err != nil {
		t.Fatal(err)
	}
	if codec != CompressionZstd || len(stored) >= len(text) {
		t.Fatalf("text not compressed: codec=%d sizes=%d/%d", codec, len(stored), len(text))
	}
	got, err := Decompress(stored, codec)
	if err != nil || !bytes.Equal(got, text) {
		t.Fatalf("round trip failed: %v", err)
	}
	r := rand.New(rand.NewSource(42))
	raw := make([]byte, 64<<10)
	if _, err = r.Read(raw); err != nil {
		t.Fatal(err)
	}
	stored, codec, err = Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if codec != CompressionNone || !bytes.Equal(stored, raw) {
		t.Fatalf("high entropy payload should remain unchanged")
	}
}
