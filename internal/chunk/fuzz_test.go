package chunk

import (
	"bytes"
	"testing"
)

// Split must cover the input exactly, in order, with every chunk's hash
// and size matching its bytes and within the size limits.
func FuzzSplit(f *testing.F) {
	f.Add([]byte(""), uint8(0))
	f.Add([]byte("hello"), uint8(1))
	f.Add(bytes.Repeat([]byte{0}, 300<<10), uint8(3))
	f.Fuzz(func(t *testing.T, seed []byte, rep uint8) {
		// Repeat the input so it spans several chunks.
		data := bytes.Repeat(seed, int(rep)%64+1)
		var got []byte
		var off int64
		err := Split(bytes.NewReader(data), func(c Chunk, b []byte) error {
			if c.Off != off || c.Len != int64(len(b)) || c.Sum != Sum(b) {
				t.Fatalf("chunk %+v doesn't match its %d bytes at offset %d", c, len(b), off)
			}
			if len(b) == 0 || len(b) > maxSize {
				t.Fatalf("chunk of %d bytes", len(b))
			}
			off += c.Len
			got = append(got, b...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("chunks don't add up to the input")
		}
	})
}
