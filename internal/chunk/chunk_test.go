package chunk

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestChunksSurviveInsertion(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	old := make([]byte, 8<<20)
	rng.Read(old)
	// New version: 100 bytes inserted near the start, a block changed in the middle.
	ins := make([]byte, 100)
	rng.Read(ins)
	nw := append(append(append([]byte{}, old[:1000]...), ins...), old[1000:]...)
	rng.Read(nw[4<<20 : 4<<20+5000])

	split := func(b []byte) []Chunk {
		var cs []Chunk
		if err := Split(bytes.NewReader(b), func(c Chunk, data []byte) error {
			if Sum(data) != c.Sum || int64(len(data)) != c.Len {
				t.Fatal("chunk data mismatch")
			}
			cs = append(cs, c)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return cs
	}
	a, b := split(old), split(nw)
	var total int64
	for _, c := range b {
		if c.Len > maxSize || (c.Len < minSize && c.Off+c.Len != int64(len(nw))) {
			t.Fatalf("chunk size %d out of bounds", c.Len)
		}
		total += c.Len
	}
	if total != int64(len(nw)) {
		t.Fatal("chunks don't cover the file")
	}
	have := map[string]bool{}
	for _, c := range a {
		have[c.Sum] = true
	}
	var missing int64
	for _, c := range b {
		if !have[c.Sum] {
			missing += c.Len
		}
	}
	if avg := len(nw) / len(b); avg < 40<<10 || avg > 100<<10 {
		t.Errorf("average chunk %d bytes", avg)
	}
	if missing > 1<<20 {
		t.Fatalf("a small edit changed %d bytes of chunks", missing)
	}
	t.Logf("%d chunks, %d KB to fetch of %d KB", len(b), missing>>10, len(nw)>>10)
}
