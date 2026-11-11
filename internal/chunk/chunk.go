// Package chunk splits files into content-defined chunks (a gear-hash CDC in
// the style of FastCDC). Because cut points depend on content, not offsets,
// an insertion early in a file only changes the chunks around it, so two
// versions of an artifact share most chunks. Agents use this for delta
// updates: they rebuild a new release from chunks of the version they
// already have and download only the rest.
package chunk

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Algorithm names the chunking parameters; server and agent must agree.
const Algorithm = "gear-cdc-16k-64k-256k-v1"

const (
	minSize = 16 << 10
	avgSize = 64 << 10
	maxSize = 256 << 10
	// Normalised chunking: a stricter mask before the average size and a
	// looser one after it narrows the size distribution.
	maskS = uint64(0xFFFFC00000000000) // 18 bits
	maskL = uint64(0xFFFC000000000000) // 14 bits
)

var gear = func() (t [256]uint64) {
	x := uint64(0x6a09e667f3bcc908) // splitmix64, fixed seed
	for i := range t {
		x += 0x9e3779b97f4a7c15
		z := x
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		t[i] = z ^ (z >> 31)
	}
	return
}()

// Chunk is one piece of a file.
type Chunk struct {
	Off int64
	Len int64
	Sum string // first 16 bytes of SHA-256, hex
}

// Sum hashes chunk data the way Split does.
func Sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}

// Split reads r to the end and calls fn for each chunk in order.
func Split(r io.Reader, fn func(Chunk, []byte) error) error {
	br := bufio.NewReaderSize(r, 1<<20)
	cur := make([]byte, 0, maxSize)
	var off int64
	var h uint64
	emit := func() error {
		if len(cur) == 0 {
			return nil
		}
		c := Chunk{Off: off, Len: int64(len(cur)), Sum: Sum(cur)}
		if err := fn(c, cur); err != nil {
			return err
		}
		off += c.Len
		cur, h = cur[:0], 0
		return nil
	}
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			return emit()
		} else if err != nil {
			return err
		}
		cur = append(cur, b)
		h = (h << 1) + gear[b]
		n := len(cur)
		if n < minSize {
			continue
		}
		mask := maskL
		if n < avgSize {
			mask = maskS
		}
		if h&mask == 0 || n >= maxSize {
			if err := emit(); err != nil {
				return err
			}
		}
	}
}

// File chunks a file.
func File(path string) ([]Chunk, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Chunk
	err = Split(f, func(c Chunk, _ []byte) error {
		out = append(out, c)
		return nil
	})
	return out, err
}
