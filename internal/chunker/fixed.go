package chunker

import (
	"fmt"
	"io"
)

const DefaultChunkSize = 4 << 20 // 4 MiB

// Fixed splits its input into equal-sized chunks (except the last one).
// Using a fixed size is simple to reason about and easy to explain in an
// interview: every chunk boundary is predictable, so dedup only works across
// identical block-aligned regions, which is good enough for the prototype.
type Fixed struct {
	ChunkSize int
}

// NewFixed returns a Fixed chunker using the given chunk size in bytes.
// Pass DefaultChunkSize for the spec-required 4 MiB.
func NewFixed(size int) *Fixed {
	if size <= 0 {
		size = DefaultChunkSize
	}
	return &Fixed{ChunkSize: size}
}

// Split reads r fully and returns one Chunk per block.  The chunkID function
// is supplied by the caller so the chunker itself stays key-agnostic.
func (f *Fixed) Split(r io.Reader, chunkID func([]byte) string) ([]Chunk, error) {
	var chunks []Chunk
	buf := make([]byte, f.ChunkSize)

	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			// Copy the slice so each Chunk owns its memory independently.
			data := make([]byte, n)
			copy(data, buf[:n])
			chunks = append(chunks, Chunk{
				ID:   chunkID(data),
				Data: data,
			})
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("chunker read: %w", err)
		}
	}

	return chunks, nil
}
