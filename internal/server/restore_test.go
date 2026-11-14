package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

// rewrap rebuilds a backup archive, letting edit drop, change or add entries.
func rewrap(t *testing.T, archive []byte, edit func(name string, data []byte) (string, []byte, bool), extra map[string]string) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	tw := tar.NewWriter(zw)
	write := func(name string, data []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
	}
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		if name, d, keep := edit(hd.Name, data); keep {
			write(name, d)
		}
	}
	for name, data := range extra {
		write(name, []byte(data))
	}
	tw.Close()
	zw.Close()
	return out.Bytes()
}

func TestRestoreAndVerifyBackup(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "keeper", Info: api.DeviceInfo{OS: "linux"}}, nil)
	upload(t, ts, "app", "1.0.0")
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "ops@vendor.com", Role: api.RoleAdmin, Password: "long-password-1"}, nil)
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/backup", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("backup: %v", err)
	}
	archive, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Verify: a dry run in a temporary directory.
	info, err := VerifyBackup(bytes.NewReader(archive), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Devices != 1 || info.Releases != 1 || info.Artifacts != 1 || info.Users != 1 || info.SchemaVersion < 9 || len(info.SigningKey) != 16 {
		t.Fatalf("verify: %+v", info)
	}

	// Never over existing data.
	busy := t.TempDir()
	os.WriteFile(filepath.Join(busy, "armada.db"), []byte("live data"), 0o600)
	if _, err := RestoreBackup(bytes.NewReader(archive), busy); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("restored over existing data: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(busy, "armada.db")); string(b) != "live data" {
		t.Fatal("existing data touched")
	}

	// Damaged or doctored archives are refused, leaving nothing behind.
	for name, bad := range map[string][]byte{
		"path traversal": rewrap(t, archive, func(n string, d []byte) (string, []byte, bool) { return n, d, true }, map[string]string{"../escape": "x"}),
		"unknown file":   rewrap(t, archive, func(n string, d []byte) (string, []byte, bool) { return n, d, true }, map[string]string{"artifacts/../../x": "x"}),
		"no secrets key": rewrap(t, archive, func(n string, d []byte) (string, []byte, bool) { return n, d, n != "secrets.key" }, nil),
		"bad signing key": rewrap(t, archive, func(n string, d []byte) (string, []byte, bool) {
			if n == "signing.key" {
				return n, []byte("not a key"), true
			}
			return n, d, true
		}, nil),
		"missing artifact": rewrap(t, archive, func(n string, d []byte) (string, []byte, bool) { return n, d, !strings.HasPrefix(n, "artifacts/") }, nil),
		"truncated":        archive[:len(archive)/2],
		"not an archive":   []byte("<html>login page</html>"),
	} {
		dir := filepath.Join(t.TempDir(), "data")
		if _, err := RestoreBackup(bytes.NewReader(bad), dir); err == nil {
			t.Errorf("%s: restored", name)
			continue
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: left %d files behind", name, len(entries))
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
			t.Errorf("%s: wrote outside the data directory", name)
		}
	}

	// A real restore: a server runs on it, with the same fleet and keys.
	dir := filepath.Join(t.TempDir(), "restored")
	if _, err := RestoreBackup(bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "signing.key")); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v", st.Mode())
	}
	_, ts2 := setupWith(t, func(c *Config) { c.DataDir = dir })
	var devs []api.Device
	call(t, ts2, admin, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 1 || devs[0].Name != "keeper" {
		t.Fatalf("restored devices: %+v", devs)
	}
	var k1, k2 map[string]string
	call(t, ts, admin, "GET", "/api/v1/signing-key", nil, &k1)
	call(t, ts2, admin, "GET", "/api/v1/signing-key", nil, &k2)
	if k1["public_key"] == "" || k1["public_key"] != k2["public_key"] {
		t.Fatalf("signing key changed: %v vs %v", k1, k2)
	}
}
