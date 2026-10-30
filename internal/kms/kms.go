package kms

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
)

// KMS is the interface that wraps key-management operations.
// The real implementation calls AWS KMS; the fake is used locally and in tests.
// Keeping this as an interface means all crypto-path code is testable without
// AWS credentials and without any mock-generation framework.
type KMS interface {
	// GenerateDataKey asks the KMS to generate a fresh 32-byte (256-bit)
	// data key.  It returns (plaintext, ciphertext).  The plaintext key is
	// used once, briefly, then discarded; only the ciphertext is stored.
	GenerateDataKey(ctx context.Context, keyID string) (plaintext, ciphertext []byte, err error)

	// Decrypt unwraps a ciphertext data key that was previously returned by
	// GenerateDataKey and returns the plaintext.
	Decrypt(ctx context.Context, ciphertext []byte) (plaintext []byte, err error)
}

// --- Fake implementation for local dev and tests ---

// FakeKMS "encrypts" the data key by XOR-ing with a fixed byte; this is
// obviously insecure but it lets us run the full code path without AWS.
// The real KMS is a one-line swap; the interface does not change.
type FakeKMS struct{}

const fakeXorByte = 0xAB

func (FakeKMS) GenerateDataKey(_ context.Context, _ string) ([]byte, []byte, error) {
	plaintext := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, plaintext); err != nil {
		return nil, nil, fmt.Errorf("fakekms: generate key: %w", err)
	}
	ciphertext := make([]byte, len(plaintext))
	for i, b := range plaintext {
		ciphertext[i] = b ^ fakeXorByte
	}
	return plaintext, ciphertext, nil
}

func (FakeKMS) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	plaintext := make([]byte, len(ciphertext))
	for i, b := range ciphertext {
		plaintext[i] = b ^ fakeXorByte
	}
	return plaintext, nil
}
