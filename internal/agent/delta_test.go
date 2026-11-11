package agent

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countingWriter) Write(b []byte) (int, error) {
	c.n.Add(int64(len(b)))
	return c.ResponseWriter.Write(b)
}

func TestDeltaUpdateDownloadsOnlyChangedChunks(t *testing.T) {
	h := newHarness(t)
	var artifactBytes atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifact") {
			w = countingWriter{w, &artifactBytes}
		}
		h.srv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(func() { proxy.CloseClientConnections(); proxy.Close() })

	appDir := t.TempDir()
	cfg := &Config{
		ServerURL:       proxy.URL,
		EnrollmentToken: h.token(),
		StateDir:        t.TempDir(),
		Packages: map[string]PackageConfig{"app": {
			InstallCommand: []string{"/bin/sh", "-c", `cp "$ARMADA_ARTIFACT" "` + appDir + `/app.img"`},
		}},
	}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkIn(t, a)

	rng := rand.New(rand.NewSource(7))
	v1 := make([]byte, 6<<20)
	rng.Read(v1)
	h.deploy(h.upload("app", "1.0.0", v1))
	checkIn(t, a)
	checkIn(t, a)
	full := artifactBytes.Load()
	if full < int64(len(v1)) {
		t.Fatalf("first install downloaded %d bytes", full)
	}

	// v2: a small insertion and a patched region, like a rebuilt image.
	v2 := append(append(append([]byte{}, v1[:2<<20]...), []byte("new config block")...), v1[2<<20:]...)
	rng.Read(v2[5<<20 : 5<<20+3000])
	h.deploy(h.upload("app", "2.0.0", v2))
	artifactBytes.Store(0)
	checkIn(t, a)
	checkIn(t, a)
	got, err := os.ReadFile(filepath.Join(appDir, "app.img"))
	if err != nil || !bytes.Equal(got, v2) {
		t.Fatal("installed artifact differs from v2")
	}
	if n := artifactBytes.Load(); n > 1<<20 {
		t.Fatalf("delta update downloaded %d bytes of %d", n, len(v2))
	} else {
		t.Logf("delta update downloaded %d KB of %d KB", n>>10, len(v2)>>10)
	}
	var det struct{ Updates []*api.DeviceUpdate }
	h.admin("GET", "/api/v1/devices/"+a.DeviceID(), nil, &det)
	if det.Updates[0].Status != api.UpdateSucceeded {
		t.Fatalf("update: %+v", det.Updates[0])
	}
}
