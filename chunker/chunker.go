// Package chunker splits a byte stream into variable-sized, content-defined
// chunks. Unlike fixed-size blocks, a chunk boundary is chosen based on the
// data itself (a rolling checksum over a sliding window), so inserting or
// deleting a few bytes only disturbs the chunk(s) touching that edit —
// everything else in the file re-aligns to the same boundaries as before
// and hashes identically. That property is what makes deduplication
// against a previous version of the file actually work.
//
// The rolling checksum is buzhash: a cyclic-XOR hash over a fixed-size
// window of the last windowSize bytes. It's deliberately windowed (each
// incoming byte's contribution is explicitly XORed back out once it falls
// outside the window) rather than left to fall off the end of the integer
// via overflow — a naive "shift left and add" gear hash looks similar but
// its low bits collapse to a constant on any input with a repeating period
// >= the mask width, which silently kills boundary detection on exactly the
// kind of repetitive real-world text/config files this tool exists to
// version. Buzhash doesn't have that failure mode.
package chunker

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/bits"
)

const (
	// MinSize / MaxSize / AvgSize follow the same shape as FastCDC/restic:
	// never cut a chunk smaller than MinSize (avoids pathological tiny
	// chunks), always force a cut by MaxSize (bounds memory + worst case),
	// and aim for AvgSize on average via the mask below.
	MinSize = 512 * 1024      // 512 KiB
	MaxSize = 8 * 1024 * 1024 // 8 MiB

	// maskBits is chosen so that 2^maskBits ~= desired average chunk size,
	// since a boundary condition (hash & mask == 0) fires on average once
	// per 2^maskBits bytes for a well-distributed hash. 21 bits ~= 2 MiB.
	maskBits = 21
	mask     = (1 << maskBits) - 1

	// windowSize is how many trailing bytes the rolling hash actually
	// "remembers". Must be < 64 for the rotate-by-windowSize trick below.
	windowSize = 48
)

// Chunk is one content-defined slice of the input, along with its SHA-256
// hash (used both as the dedup key and the on-disk filename in the store).
type Chunk struct {
	Data []byte
	Hash string
}

// Split reads r to completion and returns the ordered list of chunks that
// reconstruct it byte-for-byte when concatenated in order.
func Split(r io.Reader) ([]Chunk, error) {
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return SplitBytes(buf), nil
}

// SplitBytes is the same as Split but operates on an in-memory buffer.
func SplitBytes(data []byte) []Chunk {
	if len(data) == 0 {
		return nil
	}

	var chunks []Chunk
	start := 0
	var h uint64
	var window [windowSize]byte
	var winPos int

	for i := 0; i < len(data); i++ {
		b := data[i]
		size := i - start + 1

		if size <= windowSize {
			// Window not full yet since the last boundary -- just fold the
			// new byte in, nothing to remove.
			h = bits.RotateLeft64(h, 1) ^ buzTable[b]
		} else {
			out := window[winPos]
			h = bits.RotateLeft64(h, 1) ^ buzTable[b] ^ bits.RotateLeft64(buzTable[out], windowSize)
		}
		window[winPos] = b
		winPos = (winPos + 1) % windowSize

		atBoundary := size >= MinSize && h&mask == 0
		atMax := size >= MaxSize
		atEOF := i == len(data)-1

		if atBoundary || atMax || atEOF {
			chunks = append(chunks, newChunk(data[start:i+1]))
			start = i + 1
			h = 0
			winPos = 0
		}
	}
	return chunks
}

func newChunk(data []byte) Chunk {
	sum := sha256.Sum256(data)
	return Chunk{Data: data, Hash: hex.EncodeToString(sum[:])}
}
