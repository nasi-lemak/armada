package server

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // decode config of JPEG screenshots
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Screenshots. Privacy controls, all needed together:
//   - the server is started with -screenshots (off by default);
//   - the device's agent config has allow_screenshots: true (off by
//     default), so whoever runs the device has agreed;
//   - only operators on the device can take or view one, and each is
//     recorded in the activity log with who asked;
//   - images are deleted after -screenshot-retention (7 days by default).
// The agent uploads the image to the action's attachment; the action's
// output says what was captured.

const maxAttachment = 12 << 20

func (s *Server) attachmentPath(actionID string) (string, error) {
	if strings.ContainsAny(actionID, `/\.`) || actionID == "" {
		return "", errors.New("bad action ID")
	}
	return filepath.Join(s.cfg.DataDir, "attachments", actionID+".img"), nil
}

// handleActionAttachment stores the image a device captured for its
// screenshot action.
func (s *Server) handleActionAttachment(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	a, err := s.store.GetAction(r.Context(), r.PathValue("id"))
	if err != nil || a.DeviceID != dev.ID {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if a.Type != api.ActionScreenshot || !s.cfg.Screenshots {
		writeError(w, http.StatusForbidden, "this action takes no attachment")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAttachment))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the image is too large (12 MiB at most)")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || (format != "png" && format != "jpeg") {
		writeError(w, http.StatusBadRequest, "send a PNG or JPEG image")
		return
	}
	path, err := s.attachmentPath(a.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(path); err == nil {
		// Once only: re-uploading would also restart the retention clock.
		writeError(w, http.StatusConflict, "this screenshot was already uploaded")
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.internalError(w, err)
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"width": cfg.Width, "height": cfg.Height, "format": format, "bytes": len(body)})
}

// handleGetAttachment returns a screenshot to an operator on its device.
func (s *Server) handleGetAttachment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := s.store.GetAction(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	d, err := s.store.GetDevice(ctx, a.DeviceID)
	if err != nil || !principalFrom(ctx).sees(d) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	}
	if !needDeviceRole(w, r, d, api.RoleOperator) {
		return
	}
	path, err := s.attachmentPath(a.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.now().Sub(a.CreatedAt) > s.screenshotRetention() {
		os.Remove(path) // past retention, even if the prune hasn't run yet
		writeError(w, http.StatusGone, "this screenshot was deleted after "+roundDuration(s.screenshotRetention()))
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if s.now().Sub(a.CreatedAt) > s.screenshotRetention() {
			writeError(w, http.StatusGone, "this screenshot was deleted after "+roundDuration(s.screenshotRetention()))
		} else {
			writeError(w, http.StatusNotFound, "no image yet: the device hasn't sent it")
		}
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	ct := http.DetectContentType(data)
	if ct != "image/png" && ct != "image/jpeg" {
		writeError(w, http.StatusInternalServerError, "stored attachment isn't an image")
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="screenshot-%s-%s.%s"`, safeFilename(d.Name), a.CreatedAt.UTC().Format("20060102-150405"), strings.TrimPrefix(ct, "image/")))
	w.Write(data)
}

func safeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

func (s *Server) screenshotRetention() time.Duration {
	if s.cfg.ScreenshotRetention > 0 {
		return s.cfg.ScreenshotRetention
	}
	return 7 * 24 * time.Hour
}

// pruneAttachments deletes screenshots past their retention.
func (s *Server) pruneAttachments() {
	dir := filepath.Join(s.cfg.DataDir, "attachments")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-s.screenshotRetention())
	for _, e := range ents {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
