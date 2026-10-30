package crypto

import (
	"errors"
	"math"

	"github.com/klauspost/compress/zstd"
)

const CompressionNone int16 = 0
const CompressionZstd int16 = 1

var ErrUnknownCompression = errors.New("unknown compression codec")

// Entropy samples at most the first 64 KiB, as specified by the format plan.
func Entropy(data []byte) float64 {
	if len(data) > 64<<10 {
		data = data[:64<<10]
	}
	if len(data) == 0 {
		return 0
	}
	var counts [256]int
	for _, b := range data {
		counts[b]++
	}
	h := 0.0
	for _, n := range counts {
		if n > 0 {
			p := float64(n) / float64(len(data))
			h -= p * math.Log2(p)
		}
	}
	return h
}

// Compress skips high-entropy inputs and also rejects zstd output that is not smaller.
func Compress(data []byte) ([]byte, int16, error) {
	if Entropy(data) >= 7.5 {
		return data, CompressionNone, nil
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, CompressionNone, err
	}
	defer enc.Close()
	out := enc.EncodeAll(data, nil)
	if len(out) >= len(data) {
		return data, CompressionNone, nil
	}
	return out, CompressionZstd, nil
}

func Decompress(data []byte, codec int16) ([]byte, error) {
	if codec == CompressionNone {
		return data, nil
	}
	if codec != CompressionZstd {
		return nil, ErrUnknownCompression
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(data, nil)
}
