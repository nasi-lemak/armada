package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/chunk"
	"github.com/nasi-lemak/armada/internal/store"
)

// chunkIndexes computes each release's chunk index once and caches it next
// to the artifact.
type chunkIndexes struct {
	mu       sync.Mutex
	inflight map[string]*sync.WaitGroup
}

func (s *Server) chunkPath(releaseID string) string {
	return s.artifactPath(releaseID) + ".chunks.json"
}

// chunkIndex returns a release's chunk index, computing it if needed.
func (s *Server) chunkIndex(rel *api.Release) (*api.ChunkIndex, error) {
	if b, err := os.ReadFile(s.chunkPath(rel.ID)); err == nil {
		var idx api.ChunkIndex
		if json.Unmarshal(b, &idx) == nil && idx.Algorithm == chunk.Algorithm && idx.Size == rel.Size {
			return &idx, nil
		}
	}
	ci := &s.chunks
	ci.mu.Lock()
	if ci.inflight == nil {
		ci.inflight = map[string]*sync.WaitGroup{}
	}
	if wg := ci.inflight[rel.ID]; wg != nil { // someone is computing it
		ci.mu.Unlock()
		wg.Wait()
		return s.chunkIndex(rel)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	ci.inflight[rel.ID] = wg
	ci.mu.Unlock()
	defer func() {
		ci.mu.Lock()
		delete(ci.inflight, rel.ID)
		ci.mu.Unlock()
		wg.Done()
	}()

	cs, err := chunk.File(s.artifactPath(rel.ID))
	if err != nil {
		return nil, err
	}
	idx := &api.ChunkIndex{Algorithm: chunk.Algorithm, Size: rel.Size, Chunks: make([]api.ChunkRef, len(cs))}
	for i, c := range cs {
		idx.Chunks[i] = api.ChunkRef{Len: c.Len, Sum: c.Sum}
	}
	b, _ := json.Marshal(idx)
	tmp := s.chunkPath(rel.ID) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, s.chunkPath(rel.ID))
	}
	return idx, nil
}

// handleAgentChunks serves a release's chunk index to devices allowed to
// download it, so they can fetch only the chunks they lack.
func (s *Server) handleAgentChunks(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	id := r.PathValue("id")
	allowed, err := s.store.ReleaseOfferedToGateway(r.Context(), id, dev.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "release not offered to this device")
		return
	}
	rel, err := s.store.GetRelease(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "release not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	idx, err := s.chunkIndex(rel)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, idx)
}
