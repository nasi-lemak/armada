package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// fakeS3 keeps objects in memory and checks each request is signed and
// carries its body's SHA-256.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	fail    bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") || r.Header.Get("x-amz-date") == "" {
		http.Error(w, "<Error><Code>AccessDenied</Code><Message>unsigned</Message></Error>", 403)
		return
	}
	if f.fail {
		http.Error(w, "<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>", 403)
		return
	}
	switch r.Method {
	case "PUT":
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if r.Header.Get("x-amz-content-sha256") != hex.EncodeToString(sum[:]) {
			http.Error(w, "<Error><Code>XAmzContentSHA256Mismatch</Code></Error>", 400)
			return
		}
		f.objects[r.URL.Path] = b
	case "DELETE":
		delete(f.objects, r.URL.Path)
		w.WriteHeader(204)
	case "GET":
		prefix := "/" + strings.Trim(r.URL.Path, "/") + "/" + r.URL.Query().Get("prefix")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, strings.TrimPrefix(k, "/backups/"))
			}
		}
		sort.Strings(keys)
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-11-10T00:00:00Z</LastModified></Contents>", k)
		}
		fmt.Fprint(w, "</ListBucketResult>")
	}
}

func TestAutomaticBackups(t *testing.T) {
	fs := &fakeS3{objects: map[string][]byte{}}
	s3srv := httptest.NewServer(fs)
	defer s3srv.Close()
	dir := filepath.Join(t.TempDir(), "backups")
	s, ts := setupWith(t, func(c *Config) {
		c.Backup = BackupConfig{Dir: dir, Keep: 2, S3URL: s3srv.URL + "/backups/armada/", S3AccessKey: "AKID", S3SecretKey: "secret"}
	})
	clock := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	var st api.BackupStatus
	for i := 0; i < 3; i++ {
		if code := call(t, ts, admin, "POST", "/api/v1/backup/run", nil, &st); code != 200 || st.LastError != "" || st.LastOKAt == nil {
			t.Fatalf("run %d: %d %+v", i, code, st)
		}
		clock = clock.Add(24 * time.Hour)
	}
	// Two kept in each place, the newest.
	files, _ := filepath.Glob(filepath.Join(dir, "armada-backup-*.tar.gz"))
	if len(files) != 2 || !strings.Contains(files[0], "20261111-030000") || !strings.Contains(files[1], "20261112-030000") {
		t.Fatalf("dir: %v", files)
	}
	if len(fs.objects) != 2 || fs.objects["/backups/armada/armada-backup-20261112-030000.tar.gz"] == nil {
		t.Fatalf("s3: %v", keysOf(fs.objects))
	}
	// What was uploaded restores.
	f, _ := os.Open(files[1])
	defer f.Close()
	if _, err := VerifyBackup(f, t.TempDir()); err != nil {
		t.Fatalf("backup doesn't verify: %v", err)
	}

	// S3 refusing: the folder copy is still made, and the failure shows.
	fs.mu.Lock()
	fs.fail = true
	fs.mu.Unlock()
	call(t, ts, admin, "POST", "/api/v1/backup/run", nil, &st)
	if !strings.Contains(st.LastError, "Access Denied") || !strings.Contains(st.Warning, "failed") {
		t.Fatalf("failure: %+v", st)
	}
	var sum api.FleetSummary
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.Backup == nil || sum.Backup.Warning == "" {
		t.Fatalf("summary: %+v", sum.Backup)
	}
	// The tick retries within the hour after a failure, once S3 works again.
	fs.mu.Lock()
	fs.fail = false
	fs.mu.Unlock()
	clock = clock.Add(61 * time.Minute)
	s.backupTick(t.Context(), clock)
	s.bg.Wait()
	st = api.BackupStatus{}
	call(t, ts, admin, "GET", "/api/v1/backup/status", nil, &st)
	if st.LastError != "" || st.Warning != "" {
		t.Fatalf("retry: %+v", st)
	}
}

func TestBackupsOffWarns(t *testing.T) {
	_, ts := setup(t)
	var st api.BackupStatus
	call(t, ts, admin, "GET", "/api/v1/backup/status", nil, &st)
	if st.Enabled || !strings.Contains(st.Warning, "-backup-dir") {
		t.Fatalf("off: %+v", st)
	}
	if code := call(t, ts, admin, "POST", "/api/v1/backup/run", nil, nil); code != 400 {
		t.Fatalf("run while off: %d", code)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
