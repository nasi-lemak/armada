package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/s3"
)

// BackupConfig sets up automatic backups.
type BackupConfig struct {
	Dir   string        // keep backups in this folder
	Every time.Duration // how often (default 24h)
	Keep  int           // how many to keep in each place (default 14)
	// S3-compatible storage: a path-style URL (https://host/bucket/prefix/),
	// its region and keys.
	S3URL, S3Region, S3AccessKey, S3SecretKey string
	// EncryptTo, if set, encrypts backups for these age recipients (the
	// archive holds the server's private keys). Required for S3.
	EncryptTo []age.Recipient
}

func (c BackupConfig) enabled() bool { return c.Dir != "" || c.S3URL != "" }

func (c BackupConfig) every() time.Duration {
	if c.Every <= 0 {
		return 24 * time.Hour
	}
	return c.Every
}

func (c BackupConfig) keep() int {
	if c.Keep <= 0 {
		return 14
	}
	return c.Keep
}

// backupState is persisted in the store's settings, so a restart neither
// loses nor repeats the schedule.
type backupState struct {
	LastAt   time.Time `json:"last_at"`
	LastOKAt time.Time `json:"last_ok_at"`
	File     string    `json:"file"`
	Size     int64     `json:"size"`
	Error    string    `json:"error,omitempty"`
}

func (s *Server) loadBackupState(ctx context.Context) backupState {
	var st backupState
	if v, err := s.store.GetMeta(ctx, "auto_backup"); err == nil && v != "" {
		json.Unmarshal([]byte(v), &st)
	}
	return st
}

// backupTick starts a backup in the background when one is due.
func (s *Server) backupTick(ctx context.Context, now time.Time) {
	if !s.cfg.Backup.enabled() {
		return
	}
	st := s.loadBackupState(ctx)
	due := st.LastAt.IsZero() || now.Sub(st.LastAt) >= s.cfg.Backup.every()
	// After a failure, retry within the hour rather than waiting a whole period.
	if !due && st.Error != "" && now.Sub(st.LastAt) >= time.Hour {
		due = true
	}
	if !due || !s.backupMu.TryLock() {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.backupMu.Unlock()
		s.runAutoBackup(context.WithoutCancel(ctx))
	}()
}

// runAutoBackup makes one backup, stores it where configured and prunes
// old ones. The caller holds s.backupMu.
func (s *Server) runAutoBackup(ctx context.Context) backupState {
	now := s.now()
	st := s.loadBackupState(ctx)
	st.LastAt = now
	name := backupName(now)
	if len(s.cfg.Backup.EncryptTo) > 0 {
		name += ".age"
	}
	err := s.makeBackupFile(ctx, name, &st)
	if err != nil {
		st.Error = err.Error()
		s.log.Error("automatic backup failed", "err", err)
		s.logEvent(ctx, "server.backup_failed", "", "", "automatic backup failed: "+err.Error())
	} else {
		st.Error, st.LastOKAt, st.File = "", now, name
		s.logEvent(ctx, "server.backup", "", "", fmt.Sprintf("automatic backup %s (%s) to %s", name, bytesText(st.Size), strings.Join(s.backupDestinations(), " and ")))
	}
	if b, err := json.Marshal(st); err == nil {
		if err := s.store.SetMeta(ctx, "auto_backup", string(b)); err != nil {
			s.log.Error("save backup state", "err", err)
		}
	}
	return st
}

func (s *Server) makeBackupFile(ctx context.Context, name string, st *backupState) error {
	c := s.cfg.Backup
	dir := c.Dir
	if dir == "" {
		dir = s.cfg.DataDir // a temporary file, removed after the upload
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".backup-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	var w io.Writer = io.MultiWriter(tmp, h)
	var enc io.WriteCloser
	if len(c.EncryptTo) > 0 {
		if enc, err = age.Encrypt(w, c.EncryptTo...); err != nil {
			tmp.Close()
			return err
		}
		w = enc
	}
	if _, err := s.writeBackup(ctx, w); err != nil {
		tmp.Close()
		return err
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	size, _ := tmp.Seek(0, io.SeekCurrent)
	st.Size = size
	var errs []error
	if c.S3URL != "" {
		if err := s.uploadBackup(ctx, tmp, name, size, hex.EncodeToString(h.Sum(nil))); err != nil {
			errs = append(errs, fmt.Errorf("S3: %w", err))
		}
	}
	tmp.Close()
	if c.Dir != "" {
		if err := os.Rename(tmp.Name(), filepath.Join(c.Dir, name)); err != nil {
			errs = append(errs, err)
		} else if err := pruneBackupDir(c.Dir, c.keep()); err != nil {
			errs = append(errs, fmt.Errorf("pruning %s: %w", c.Dir, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) s3Client() (*s3.Client, error) {
	c := s.cfg.Backup
	return s3.New(c.S3URL, c.S3Region, c.S3AccessKey, c.S3SecretKey)
}

func (s *Server) uploadBackup(ctx context.Context, f *os.File, name string, size int64, sum string) error {
	cl, err := s.s3Client()
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := cl.Put(ctx, name, f, size, sum); err != nil {
		return err
	}
	objs, err := cl.List(ctx)
	if err != nil {
		return fmt.Errorf("uploaded, but listing old backups failed: %w", err)
	}
	var ours []string
	for _, o := range objs {
		if isBackupName(o.Key) {
			ours = append(ours, o.Key)
		}
	}
	sort.Strings(ours) // names sort by time
	for len(ours) > s.cfg.Backup.keep() {
		if err := cl.Delete(ctx, ours[0]); err != nil {
			return fmt.Errorf("uploaded, but removing old backup %s failed: %w", ours[0], err)
		}
		ours = ours[1:]
	}
	return nil
}

func isBackupName(n string) bool {
	return strings.HasPrefix(n, "armada-backup-") && (strings.HasSuffix(n, ".tar.gz") || strings.HasSuffix(n, ".tar.gz.age")) && !strings.Contains(n, "/")
}

// SecureURL reports whether a URL that will carry secrets (a site token,
// S3 keys, backups) is https, or http only to this machine.
func SecureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return u.Scheme == "http" && (h == "localhost" || (ip != nil && ip.IsLoopback()))
}

// pruneBackupDir keeps the newest keep backups in dir (only files we name).
func pruneBackupDir(dir string, keep int) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var ours []string
	for _, e := range ents {
		if e.Type().IsRegular() && isBackupName(e.Name()) {
			ours = append(ours, e.Name())
		}
	}
	sort.Strings(ours)
	for len(ours) > keep {
		if err := os.Remove(filepath.Join(dir, ours[0])); err != nil {
			return err
		}
		ours = ours[1:]
	}
	return nil
}

func (s *Server) backupDestinations() []string {
	var out []string
	if s.cfg.Backup.Dir != "" {
		out = append(out, s.cfg.Backup.Dir)
	}
	if s.cfg.Backup.S3URL != "" {
		if cl, err := s.s3Client(); err == nil {
			out = append(out, cl.String())
		} else {
			out = append(out, "S3 (misconfigured: "+err.Error()+")")
		}
	}
	return out
}

// backupStatus is the summary for the overview and API.
func (s *Server) backupStatus(ctx context.Context) *api.BackupStatus {
	c := s.cfg.Backup
	bs := &api.BackupStatus{Enabled: c.enabled()}
	if !bs.Enabled {
		bs.Warning = "Automatic backups are off. Set -backup-dir or -backup-s3-url so you can recover the server."
		return bs
	}
	bs.Every, bs.Keep, bs.Destination = c.every().String(), c.keep(), s.backupDestinations()
	st := s.loadBackupState(ctx)
	if !st.LastAt.IsZero() {
		bs.LastAt = &st.LastAt
	}
	if !st.LastOKAt.IsZero() {
		bs.LastOKAt = &st.LastOKAt
	}
	bs.LastFile, bs.LastSize, bs.LastError = st.File, st.Size, st.Error
	switch {
	case st.Error != "":
		bs.Warning = "The last automatic backup failed: " + st.Error
	case !st.LastOKAt.IsZero() && s.now().Sub(st.LastOKAt) > 2*c.every()+time.Hour:
		bs.Warning = fmt.Sprintf("No successful backup since %s.", st.LastOKAt.UTC().Format("2006-01-02 15:04 MST"))
	}
	return bs
}

func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backupStatus(r.Context()))
}

// handleRunBackup makes an automatic backup now and reports how it went.
func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Backup.enabled() {
		writeError(w, http.StatusBadRequest, "automatic backups aren't set up: start the server with -backup-dir or -backup-s3-url")
		return
	}
	if !s.backupMu.TryLock() {
		writeError(w, http.StatusConflict, "a backup is already running")
		return
	}
	s.runAutoBackup(context.WithoutCancel(r.Context()))
	s.backupMu.Unlock()
	writeJSON(w, http.StatusOK, s.backupStatus(r.Context()))
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
