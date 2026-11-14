package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

// writePair issues a certificate valid until notAfter and writes it and its key.
func writePair(t *testing.T, ca *certAuthority, certFile, keyFile string, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, _ := certs.GenerateKey("ecdsa-p256")
	c := ca.issue(t, key.Public(), "fleet.example", []string{"fleet.example"}, []string{"127.0.0.1"}, time.Now().Add(-time.Hour), notAfter)
	kp, _ := certs.EncodeKey(key)
	if certFile != "" {
		os.WriteFile(certFile, certs.EncodeCerts(c), 0o644)
	}
	if keyFile != "" {
		os.WriteFile(keyFile, kp, 0o600)
	}
	return c
}

func TestServingCertReloads(t *testing.T) {
	ca := newAuthority(t, "Test CA", nil)
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	first := writePair(t, ca, cf, kf, time.Now().Add(60*24*time.Hour))
	sc, err := LoadServingCert(cf, kf, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Leaf().Equal(first) {
		t.Fatal("wrong certificate loaded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Watch(ctx, 20*time.Millisecond)

	// A TLS listener configured like the real server (GetCertificate only).
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: sc.GetCertificate, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	served := func() *x509.Certificate {
		t.Helper()
		conn, err := tls.Dial("tcp", l.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}
	if !served().Equal(first) {
		t.Fatal("not serving the first certificate")
	}

	// Renewal half done (new certificate, old key): keep serving the old one.
	time.Sleep(10 * time.Millisecond) // a distinct modification time
	second := writePair(t, ca, cf, "", time.Now().Add(90*24*time.Hour))
	time.Sleep(100 * time.Millisecond)
	if !served().Equal(first) {
		t.Fatal("served a certificate without its key")
	}
	// The key arrives (certbot writes both): the new pair is served, no restart.
	key2, _ := certs.GenerateKey("ecdsa-p256")
	second = ca.issue(t, key2.Public(), "fleet.example", []string{"fleet.example"}, nil, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	kp, _ := certs.EncodeKey(key2)
	os.WriteFile(cf, certs.EncodeCerts(second), 0o644)
	os.WriteFile(kf, kp, 0o600)
	deadline := time.Now().Add(3 * time.Second)
	for !served().Equal(second) {
		if time.Now().After(deadline) {
			t.Fatal("renewed certificate not picked up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sc.Leaf().Equal(second) {
		t.Fatal("Leaf not updated")
	}
	// Explicit reload with broken files fails and keeps the current one.
	os.WriteFile(kf, []byte("garbage"), 0o600)
	if err := sc.Reload(); err == nil {
		t.Fatal("reloaded a broken key")
	}
	if !served().Equal(second) {
		t.Fatal("lost the certificate after a failed reload")
	}
}

func TestServerCertWarningsAndAlert(t *testing.T) {
	ca := newAuthority(t, "Test CA", nil)
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writePair(t, ca, cf, kf, time.Now().Add(10*24*time.Hour))
	sc, err := LoadServingCert(cf, kf, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, ts := setupWith(t, func(c *Config) { c.ServingCert = sc })
	hook, hooks := captureServer(t)

	var sum api.FleetSummary
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.ServerCert == nil || sum.ServerCert.Source != "tls-cert" || !sum.ServerCert.Warning || *sum.ServerCert.DaysLeft != 9 {
		t.Fatalf("summary: %+v", sum.ServerCert)
	}

	// Client accounts don't see it, and can't alert on it.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	var csum api.FleetSummary
	call(t, ts, lr.Token, "GET", "/api/v1/summary", nil, &csum)
	if csum.ServerCert != nil {
		t.Fatal("client sees the server certificate")
	}
	var ch api.NotificationChannel
	call(t, ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "ops", Type: "webhook", Webhook: &api.WebhookConfig{URL: hook.URL}}, &ch)
	if code := call(t, ts, lr.Token, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Enabled: true, Condition: api.ConditionServerCertExpiry,
		ClientScope: api.Selector{Labels: map[string]string{"customer": "acme"}}}, nil); code != 400 && code != 403 {
		t.Fatalf("client server_cert_expiry rule: %d", code)
	}

	if code := call(t, ts, admin, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "Server certificate", Enabled: true,
		Condition: api.ConditionServerCertExpiry, Channels: []string{ch.ID}}, nil); code != 201 {
		t.Fatalf("rule: %d", code)
	}
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	n := notification(t, next(t, hooks))
	if n.Severity != api.HealthWarning || !strings.Contains(n.Summary, "server's TLS certificate (fleet.example) expires in 9 days") {
		t.Fatalf("alert: %+v", n)
	}
}

func TestServerCertBehindProxy(t *testing.T) {
	// The public URL is served by "a proxy" (httptest's TLS server).
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer proxy.Close()
	s, ts := setupWith(t, func(c *Config) {
		c.Android.PublicURL = proxy.URL
		c.CheckPublicCert = true
	})
	pool := x509.NewCertPool()
	pool.AddCert(proxy.Certificate())
	s.serverCertRoots = pool
	var sum api.FleetSummary
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	st := sum.ServerCert
	if st == nil || st.Source != "public-url" || st.Certificate == nil || st.Error != "" || st.DaysLeft == nil {
		t.Fatalf("proxied: %+v", st)
	}

	// Without the test root, devices wouldn't trust it: that's reported.
	s.serverCertRoots = nil
	s.serverCert.probed = time.Time{}
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if st := sum.ServerCert; st == nil || !strings.HasPrefix(st.Error, "devices may not trust it") || st.Certificate == nil {
		t.Fatalf("untrusted: %+v", st)
	}

	// Off by default (tests and plain-HTTP setups never dial out).
	_, ts2 := setupWith(t, func(c *Config) { c.Android.PublicURL = proxy.URL })
	var sum2 api.FleetSummary
	call(t, ts2, admin, "GET", "/api/v1/summary", nil, &sum2)
	if sum2.ServerCert != nil {
		t.Fatalf("checked without CheckPublicCert: %+v", sum2.ServerCert)
	}
}
