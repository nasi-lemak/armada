package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/chunk"
)

// minDeltaSize is the smallest artifact worth a delta download.
const minDeltaSize = 1 << 20

// errDeltaNotWorthIt means too little could be reused; download in full.
var errDeltaNotWorthIt = errors.New("too few chunks in common")

// deltaSeed is a local file likely to share content with the new release:
// the package's current artifact, or for agent updates the running binary.
func (a *Agent) deltaSeed(u api.UpdateInstruction) string {
	if u.Peripheral != nil || u.Size < minDeltaSize {
		return ""
	}
	seed := ""
	if u.Package == api.AgentPackage {
		seed = a.ExePath
	} else {
		seed = a.state.get().Installed[u.Package].Artifact
	}
	if seed == "" {
		return ""
	}
	if st, err := os.Stat(seed); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	return seed
}

// deltaDownload rebuilds a release in dst from the chunks it shares with
// seed, downloading only the missing chunks with HTTP range requests. The
// caller still verifies the whole file's SHA-256 and signature.
func (c *client) deltaDownload(ctx context.Context, u api.UpdateInstruction, seed, dst string) (string, error) {
	var idx api.ChunkIndex
	if err := c.do(ctx, "GET", "/api/v1/agent/releases/"+u.ReleaseID+"/chunks", nil, &idx); err != nil {
		return "", err
	}
	if idx.Algorithm != chunk.Algorithm || idx.Size != u.Size {
		return "", fmt.Errorf("chunk index is %s for %d bytes", idx.Algorithm, idx.Size)
	}
	local := map[string]chunk.Chunk{}
	seedChunks, err := chunk.File(seed)
	if err != nil {
		return "", err
	}
	for _, sc := range seedChunks {
		local[sc.Sum] = sc
	}
	var missing int64
	for _, ref := range idx.Chunks {
		if _, ok := local[ref.Sum]; !ok {
			missing += ref.Len
		}
	}
	if missing*10 > idx.Size*7 {
		return "", errDeltaNotWorthIt
	}

	src, err := os.Open(seed)
	if err != nil {
		return "", err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(dst)
		}
	}()
	buf := make([]byte, 0, 256<<10)
	var off int64 // offset in the new artifact
	for i := 0; i < len(idx.Chunks); {
		ref := idx.Chunks[i]
		if lc, have := local[ref.Sum]; have && lc.Len == ref.Len {
			buf = buf[:ref.Len]
			if _, err := src.ReadAt(buf, lc.Off); err != nil {
				return "", err
			}
			if _, err := out.Write(buf); err != nil {
				return "", err
			}
			off += ref.Len
			i++
			continue
		}
		// A run of missing chunks: one range request.
		j, runLen := i, int64(0)
		for j < len(idx.Chunks) {
			if _, have := local[idx.Chunks[j].Sum]; have {
				break
			}
			runLen += idx.Chunks[j].Len
			j++
		}
		if err := c.fetchRange(ctx, u.ArtifactPath, off, runLen, idx.Chunks[i:j], out); err != nil {
			return "", err
		}
		off += runLen
		i = j
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	ok = true
	return fmt.Sprintf("delta update: downloaded %s of %s (%d%% reused)", humanSize(missing), humanSize(idx.Size), 100-int(100*missing/max(idx.Size, 1))), nil
}

// fetchRange downloads bytes [off, off+n) and writes them to w, checking
// each chunk's hash on the way.
func (c *client) fetchRange(ctx context.Context, path string, off, n int64, refs []api.ChunkRef, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key())
	req.Header.Set("User-Agent", "armada-agent/"+Version)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	resp, err := c.download.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range request: HTTP %d", resp.StatusCode)
	}
	buf := make([]byte, 0, 256<<10)
	for _, ref := range refs {
		buf = buf[:ref.Len]
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			return err
		}
		if chunk.Sum(buf) != ref.Sum {
			return errors.New("downloaded chunk does not match the index")
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
