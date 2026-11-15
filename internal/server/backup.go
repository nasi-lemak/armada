package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// writeBackup writes a .tar.gz with everything needed to restore the
// server: a consistent database snapshot, the release, license and
// secrets keys, and all release artifacts. Restore by extracting it into
// an empty data directory (armada-server -restore). The archive contains
// private keys; store it securely.
func (s *Server) writeBackup(ctx context.Context, w io.Writer) (files int, err error) {
	tmp, err := os.CreateTemp(s.cfg.DataDir, "backup-*.db")
	if err != nil {
		return 0, err
	}
	snap := tmp.Name()
	tmp.Close()
	os.Remove(snap) // VACUUM INTO needs a path that doesn't exist yet
	defer os.Remove(snap)
	if err := s.store.Snapshot(ctx, snap); err != nil {
		return 0, err
	}
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
	list := [][2]string{{snap, "armada.db"}, {filepath.Join(s.cfg.DataDir, "signing.key"), "signing.key"}, {filepath.Join(s.cfg.DataDir, "license.key"), "license.key"}, {filepath.Join(s.cfg.DataDir, "secrets.key"), "secrets.key"}}
	arts, _ := os.ReadDir(s.artifactDir)
	for _, a := range arts {
		// Artifacts are stored under their release ID; skip in-progress uploads.
		if a.Type().IsRegular() && !strings.HasPrefix(a.Name(), "upload-") && !strings.HasSuffix(a.Name(), ".chunks.json") && !strings.HasSuffix(a.Name(), ".tmp") {
			list = append(list, [2]string{filepath.Join(s.artifactDir, a.Name()), "artifacts/" + a.Name()})
		}
	}
	for _, f := range list {
		if err := add(f[0], f[1]); err != nil {
			return 0, fmt.Errorf("%s: %w", f[1], err)
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return len(list), gz.Close()
}

func backupName(t time.Time) string {
	return "armada-backup-" + t.UTC().Format("20060102-150405") + ".tar.gz"
}

// handleBackup streams a backup to the caller.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+backupName(s.now())+`"`)
	n, err := s.writeBackup(r.Context(), w)
	if err != nil {
		// Headers are already sent; abort the stream so the client sees a broken archive.
		s.log.Error("backup failed", "err", err)
		panic(http.ErrAbortHandler)
	}
	s.logEvent(r.Context(), "server.backup", "", "", fmt.Sprintf("backup downloaded (%d files)", n))
}
