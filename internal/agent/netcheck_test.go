package agent

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func netCheckAgent(t *testing.T, serverURL, caFile string) *Agent {
	t.Helper()
	cfg := &Config{ServerURL: serverURL, CACert: caFile}
	c, err := newClient(cfg, func() string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{cfg: cfg, client: c}
}

func TestNetCheck(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)

	// Trusted: every step passes.
	report, ok := netCheckAgent(t, srv.URL, ca).netCheck(t.Context())
	for _, want := range []string{"DNS", "TCP", "TLS", "handshake", "HTTPS", "/healthz 200", "3 of 3 tries", "Result: OK"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if !ok {
		t.Fatalf("not ok:\n%s", report)
	}

	// Untrusted certificate: it stops at TLS and says what to do.
	report, ok = netCheckAgent(t, srv.URL, "").netCheck(t.Context())
	if ok || !strings.Contains(report, "TLS         FAILED") || !strings.Contains(report, "ca_cert") {
		t.Fatalf("untrusted:\n%s", report)
	}

	// Nothing listening: it stops at TCP.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	report, ok = netCheckAgent(t, "https://"+addr, "").netCheck(t.Context())
	if ok || !strings.Contains(report, "TCP         FAILED") || !strings.Contains(report, "connection refused") {
		t.Fatalf("closed port:\n%s", report)
	}
}
