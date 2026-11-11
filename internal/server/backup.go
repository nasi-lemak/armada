package server

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleBackup streams a .tar.gz with everything needed to restore the
// server: a consistent database snapshot, the release and license signing
// keys, and all release artifacts. Restore by extracting it into an empty
// data directory. The archive contains private keys; store it securely.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.CreateTemp(s.cfg.DataDir, "backup-*.db")
	if err != nil {
		s.internalError(w, err)
		return
	}
	snap := tmp.Name()
	tmp.Close()
	os.Remove(snap) // VACUUM INTO needs a path that doesn't exist yet
	defer os.Remove(snap)
	if err := s.store.Snapshot(r.Context(), snap); err != nil {
		s.internalError(w, err)
		return
	}
	name := "armada-backup-" + s.now().Format("20060102-150405") + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(src, dst string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: dst, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	}
	files := [][2]string{{snap, "armada.db"}, {filepath.Join(s.cfg.DataDir, "signing.key"), "signing.key"}, {filepath.Join(s.cfg.DataDir, "license.key"), "license.key"}}
	arts, _ := os.ReadDir(s.artifactDir)
	for _, a := range arts {
		// Artifacts are stored under their release ID; skip in-progress uploads.
		if a.Type().IsRegular() && !strings.HasPrefix(a.Name(), "upload-") {
			files = append(files, [2]string{filepath.Join(s.artifactDir, a.Name()), "artifacts/" + a.Name()})
		}
	}
	for _, f := range files {
		if err := add(f[0], f[1]); err != nil {
			// Headers are already sent; abort the stream so the client sees a broken archive.
			s.log.Error("backup failed", "file", f[1], "err", err)
			panic(http.ErrAbortHandler)
		}
	}
	if err := tw.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	if err := gz.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	s.logEvent(r.Context(), "server.backup", "", "", fmt.Sprintf("backup downloaded (%d files)", len(files)))
}
