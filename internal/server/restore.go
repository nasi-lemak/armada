package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nasi-lemak/armada/internal/store"
)

// Restoring a backup (GET /api/v1/backup, armadactl backup): extract it
// into a fresh data directory and check the result can run, before
// starting a server on it. VerifyBackup does the same in a temporary
// directory, to test backups without touching anything.

// RestoreInfo describes a restored or verified backup.
type RestoreInfo struct {
	SchemaVersion int
	Devices       int
	Releases      int
	Users         int
	Artifacts     int
	// SigningKey identifies the release-signing key (SHA-256 of the public
	// key): devices that pinned a key need this same one.
	SigningKey string
}

var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// backupFiles are the files a backup holds besides artifacts.
var backupFiles = []string{"armada.db", "signing.key", "license.key", "secrets.key"}

// RestoreBackup extracts a backup into dataDir, which must be empty or not
// exist yet, and checks it. If anything fails, what was written is removed.
func RestoreBackup(archive io.Reader, dataDir string) (*RestoreInfo, error) {
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty: restore into a new data directory (stop the server, move the old one aside)", dataDir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	info, err := restoreInto(archive, dataDir)
	if err != nil {
		cleanDir(dataDir)
		return nil, fmt.Errorf("%w (the partly restored files were removed)", err)
	}
	return info, nil
}

// VerifyBackup restores a backup into a temporary directory under tmpParent
// (the system's temporary directory if empty), checks it, and removes it.
func VerifyBackup(archive io.Reader, tmpParent string) (*RestoreInfo, error) {
	dir, err := os.MkdirTemp(tmpParent, "armada-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	return restoreInto(archive, dir)
}

func cleanDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func restoreInto(archive io.Reader, dir string) (*RestoreInfo, error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("not a backup archive (.tar.gz): %w", err)
	}
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	artifacts := map[string]bool{}
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is damaged or incomplete: %w", err)
		}
		if hd.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("unexpected entry %q in the archive", hd.Name)
		}
		// Only the files a backup contains, at their own paths.
		name, ok := hd.Name, false
		if containsStr(backupFiles, name) {
			ok = true
		} else if a, found := strings.CutPrefix(name, "artifacts/"); found && artifactName.MatchString(a) {
			ok = true
			artifacts[a] = true
			if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
				return nil, err
			}
		}
		if !ok || seen[name] {
			return nil, fmt.Errorf("unexpected entry %q in the archive", hd.Name)
		}
		seen[name] = true
		f, err := os.OpenFile(filepath.Join(dir, filepath.FromSlash(name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("extracting %s: %w", name, err)
		}
	}
	for _, f := range backupFiles {
		if !seen[f] {
			return nil, fmt.Errorf("the archive has no %s: not a complete Armada backup", f)
		}
	}
	return checkDataDir(dir, artifacts)
}

// checkDataDir checks a restored data directory: keys readable, database
// intact, every release's artifact present.
func checkDataDir(dir string, artifacts map[string]bool) (*RestoreInfo, error) {
	info := &RestoreInfo{Artifacts: len(artifacts)}
	for _, k := range []string{"signing.key", "license.key"} {
		b, err := os.ReadFile(filepath.Join(dir, k))
		if err != nil {
			return nil, err
		}
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s in the backup is corrupt", k)
		}
		if k == "signing.key" {
			sum := sha256.Sum256(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
			info.SigningKey = hex.EncodeToString(sum[:8])
		}
	}
	if _, err := loadSecretBox(filepath.Join(dir, "secrets.key")); err != nil {
		return nil, err
	}

	// Opening applies any newer migrations, as a server would on start.
	st, err := store.Open(filepath.Join(dir, "armada.db"))
	if err != nil {
		return nil, fmt.Errorf("the database in the backup doesn't open: %w", err)
	}
	defer st.Close()
	ctx := context.Background()
	db := st.DB()
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
		return nil, fmt.Errorf("the database in the backup is damaged: %v %s", err, check)
	}
	info.SchemaVersion = st.Migration().To
	for _, c := range []struct {
		dst   *int
		table string
	}{{&info.Devices, "devices"}, {&info.Releases, "releases"}, {&info.Users, "users"}} {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+c.table).Scan(c.dst); err != nil {
			return nil, err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM releases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var missing []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !artifacts[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%d releases have no artifact in the backup (e.g. %s): devices couldn't download them", len(missing), missing[0])
	}
	return info, rows.Err()
}

// String summarises a restore for people.
func (i *RestoreInfo) String() string {
	return fmt.Sprintf("schema version %d · %d devices · %d releases (%d artifacts) · %d users · signing key %s",
		i.SchemaVersion, i.Devices, i.Releases, i.Artifacts, i.Users, i.SigningKey)
}
