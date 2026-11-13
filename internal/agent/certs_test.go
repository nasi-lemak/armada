package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

// testCA signs CSRs like a client's CA would.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Client CA"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{c, key}
}

// sign issues a certificate for a CSR (PEM in, PEM out).
func (ca *testCA) sign(t *testing.T, csrPEM string, serial int64, days int) string {
	t.Helper()
	csr, err := certs.ParseCSR([]byte(csrPEM))
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: csr.Subject, DNSNames: csr.DNSNames, IPAddresses: csr.IPAddresses,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return string(certs.EncodeCerts(c))
}

// installFirst puts an initial certificate on disk, as an installer would.
func installFirst(t *testing.T, ca *testCA, m ManagedCert, cn string) {
	t.Helper()
	key, _ := certs.GenerateKey("ecdsa-p256")
	csr, _ := certs.MakeCSR(key, api.CertSubject{CommonName: cn, Organization: "Acme"}, []string{cn}, []string{"10.0.0.7"})
	kp, _ := certs.EncodeKey(key)
	os.MkdirAll(filepath.Dir(m.Cert), 0o755)
	os.WriteFile(m.Key, kp, 0o600)
	os.WriteFile(m.Cert, []byte(ca.sign(t, string(csr), 100, 30)), 0o644)
}

// serveFromFiles is a TLS service that loads its certificate on each
// connection, like a service that re-reads it on reload.
func serveFromFiles(t *testing.T, m ManagedCert) string {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		c, err := tls.LoadX509KeyPair(m.Cert, m.Key)
		return &c, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func TestManagedCertRenewal(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	m := ManagedCert{Cert: filepath.Join(dir, "certs", "server.crt"), Key: filepath.Join(dir, "certs", "server.key"),
		FullChain: true, ReloadCommand: []string{"true"}}
	installFirst(t, ca, m, "broker-07.acme.local")
	m.VerifyAddress = serveFromFiles(t, m)
	cfg := &Config{ServerURL: "http://x", StateDir: t.TempDir(), ManagedCerts: map[string]ManagedCert{"mosquitto": m}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: cfg}
	ctx := context.Background()

	// Reports: the installed certificate, its expiry and that the key matches.
	rep := a.certReports()
	if len(rep) != 1 || rep[0].Info == nil || rep[0].Info.CommonName != "broker-07.acme.local" || !rep[0].Info.KeyMatches || rep[0].Info.KeyType != "ecdsa-p256" {
		t.Fatalf("report: %+v %+v", rep, rep[0].Info)
	}

	// CSR: new key on the device, nothing else touched.
	spec := api.CertCSRSpec{Target: "mosquitto", RenewalID: "rnw1", Subject: api.CertSubject{CommonName: "broker-07.acme.local", Organization: "Acme"},
		DNSNames: []string{"broker-07.acme.local"}, IPAddresses: []string{"10.0.0.7"}, KeyType: "rsa-2048"}
	csrPEM, err := a.certCSR(spec)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := certs.ParseCSR([]byte(csrPEM))
	if err != nil || csr.Subject.CommonName != "broker-07.acme.local" || csr.IPAddresses[0].String() != "10.0.0.7" || certs.KeyTypeOf(csr.PublicKey) != "rsa-2048" {
		t.Fatalf("csr: %v %+v", err, csr)
	}
	if st, err := os.Stat(a.pendingKeyPath("mosquitto", "rnw1")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("pending key: %v", err)
	}
	if strings.Contains(csrPEM, "PRIVATE KEY") {
		t.Fatal("private key in the CSR output")
	}

	// A certificate for some other key is refused.
	other, _ := certs.GenerateKey("ecdsa-p256")
	otherCSR, _ := certs.MakeCSR(other, api.CertSubject{CommonName: "x"}, nil, nil)
	if _, err := a.certInstall(ctx, api.CertInstallSpec{Target: "mosquitto", RenewalID: "rnw1", Certificate: ca.sign(t, string(otherCSR), 5, 30)}); err == nil ||
		!strings.Contains(err.Error(), "not issued for this device's key") {
		t.Fatalf("foreign certificate: %v", err)
	}

	// A reload that fails puts the old certificate back.
	signed := ca.sign(t, csrPEM, 101, 365)
	oldCert, _ := os.ReadFile(m.Cert)
	a.cfg.ManagedCerts["mosquitto"] = func() ManagedCert { x := m; x.ReloadCommand = []string{"false"}; return x }()
	if _, err := a.certInstall(ctx, api.CertInstallSpec{Target: "mosquitto", RenewalID: "rnw1", Certificate: signed, Chain: string(certs.EncodeCerts(ca.cert))}); err == nil ||
		!strings.Contains(err.Error(), "previous certificate restored") {
		t.Fatalf("failed reload: %v", err)
	}
	if now, _ := os.ReadFile(m.Cert); string(now) != string(oldCert) {
		t.Fatal("old certificate not restored")
	}

	// It works: written with the chain, service verified, pending key gone.
	a.cfg.ManagedCerts["mosquitto"] = m
	info, err := a.certInstall(ctx, api.CertInstallSpec{Target: "mosquitto", RenewalID: "rnw1", Certificate: signed, Chain: string(certs.EncodeCerts(ca.cert))})
	if err != nil {
		t.Fatal(err)
	}
	if info.Serial != "65" || !info.KeyMatches {
		t.Fatalf("installed: %+v", info)
	}
	written, _ := certs.ParseCerts(func() []byte { b, _ := os.ReadFile(m.Cert); return b }())
	if len(written) != 2 || written[1].Subject.CommonName != "Client CA" {
		t.Fatalf("full chain not written: %d certs", len(written))
	}
	if st, _ := os.Stat(m.Key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v", st.Mode())
	}
	if _, err := os.Stat(a.pendingKeyPath("mosquitto", "rnw1")); !os.IsNotExist(err) {
		t.Fatal("pending key left behind")
	}
	if rep := a.certReports(); rep[0].Info.Serial != "65" || !rep[0].Info.KeyMatches {
		t.Fatalf("report after install: %+v", rep[0].Info)
	}
	// Installing again: the key is gone.
	if _, err := a.certInstall(ctx, api.CertInstallSpec{Target: "mosquitto", RenewalID: "rnw1", Certificate: signed}); err == nil {
		t.Fatal("installed twice")
	}
}

func TestManagedCertVerifyFailureRollsBack(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	m := ManagedCert{Cert: filepath.Join(dir, "server.crt"), Key: filepath.Join(dir, "server.key")}
	installFirst(t, ca, m, "old.acme.local")
	// A service that never picks up the new certificate (it loaded the old one at start).
	pair, err := tls.LoadX509KeyPair(m.Cert, m.Key)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
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
	m.VerifyAddress = l.Addr().String()
	defer func(d time.Duration) { certVerifyTimeout = d }(certVerifyTimeout)
	certVerifyTimeout = 2 * time.Second
	a := &Agent{cfg: &Config{StateDir: t.TempDir(), ManagedCerts: map[string]ManagedCert{"web": m}}}
	csr, err := a.certCSR(api.CertCSRSpec{Target: "web", RenewalID: "r2", Subject: api.CertSubject{CommonName: "new.acme.local"}, KeyType: "ecdsa-p256"})
	if err != nil {
		t.Fatal(err)
	}
	oldKey, _ := os.ReadFile(m.Key)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = a.certInstall(ctx, api.CertInstallSpec{Target: "web", RenewalID: "r2", Certificate: ca.sign(t, csr, 7, 90)})
	if err == nil || !strings.Contains(err.Error(), "still presents the old certificate") {
		t.Fatalf("verify: %v", err)
	}
	if now, _ := os.ReadFile(m.Key); string(now) != string(oldKey) {
		t.Fatal("old key not restored")
	}
	if _, err := net.Dial("tcp", m.VerifyAddress); err != nil {
		t.Fatal(err)
	}
}

func TestManagedCertConfigValidation(t *testing.T) {
	for name, mc := range map[string]map[string]ManagedCert{
		"relative":   {"x": {Cert: "server.crt", Key: "/k"}},
		"bad name":   {"../x": {Cert: "/c", Key: "/k"}},
		"bad verify": {"x": {Cert: "/c", Key: "/k", VerifyAddress: "localhost"}},
	} {
		cfg := &Config{ServerURL: "http://x", ManagedCerts: mc}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestCertRenewalEndToEnd runs the whole flow against a real server: the
// agent reports its certificate, a renewal makes a CSR on the device, the
// "client" signs it, the signed certificate is uploaded and installed.
func TestCertRenewalEndToEnd(t *testing.T) {
	h := newHarness(t)
	ca := newTestCA(t)
	dir := t.TempDir()
	m := ManagedCert{Cert: filepath.Join(dir, "server.crt"), Key: filepath.Join(dir, "server.key"), FullChain: true,
		ReloadCommand: []string{"/bin/sh", "-c", `echo "$ARMADA_CERT_TARGET" >> ` + filepath.Join(dir, "reloads")}}
	installFirst(t, ca, m, "broker-07.acme.local")
	m.VerifyAddress = serveFromFiles(t, m)
	a := newTestAgent(t, h, &Config{EnrollmentToken: h.token(), StateDir: t.TempDir(), ManagedCerts: map[string]ManagedCert{"mosquitto": m}})
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := func() { t.Helper(); checkIn(t, a); a.wg.Wait() }
	list := func() []api.DeviceCert {
		t.Helper()
		var cs []api.DeviceCert
		h.admin("GET", "/api/v1/certificates", nil, &cs)
		return cs
	}
	run()
	cs := list()
	if len(cs) != 1 || cs[0].Target != "mosquitto" || cs[0].Installed == nil || cs[0].Installed.CommonName != "broker-07.acme.local" || cs[0].DaysLeft == nil || *cs[0].DaysLeft > 30 {
		t.Fatalf("inventory: %+v", cs)
	}

	var started api.RenewCertsResponse
	h.admin("POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true}, &started)
	if len(started.Started) != 1 {
		t.Fatalf("renew: %+v", started)
	}
	run() // the agent makes the CSR
	cs = list()
	rn := cs[0].Renewal
	if rn == nil || rn.Status != api.RenewalCSRReady || !strings.Contains(rn.CSR, "CERTIFICATE REQUEST") {
		t.Fatalf("after CSR: %+v", rn)
	}
	csr, _ := certs.ParseCSR([]byte(rn.CSR))
	if csr.Subject.CommonName != "broker-07.acme.local" || len(csr.Subject.Organization) == 0 || csr.Subject.Organization[0] != "Acme" ||
		len(csr.IPAddresses) != 1 || certs.KeyTypeOf(csr.PublicKey) != "ecdsa-p256" {
		t.Fatalf("CSR doesn't copy the installed certificate: %+v", csr.Subject)
	}

	// The client signs it; the certificate comes back with the CA after it.
	signed := ca.sign(t, rn.CSR, 777, 365)
	var up api.UploadCertsResponse
	h.admin("POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: string(certs.EncodeCerts(ca.cert)) + signed}, &up)
	if len(up.Installing) != 1 || len(up.Rejected) != 0 {
		t.Fatalf("upload: %+v", up)
	}
	run() // the agent installs, reloads, verifies
	run() // and reports the new certificate
	cs = list()
	if cs[0].Renewal.Status != api.RenewalInstalled || cs[0].Installed.Serial != "309" || *cs[0].DaysLeft < 360 {
		t.Fatalf("after install: %+v %+v", cs[0].Renewal, cs[0].Installed)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "reloads")); strings.TrimSpace(string(b)) != "mosquitto" {
		t.Fatalf("reload ran: %q", b)
	}
	var evs []api.Event
	h.admin("GET", "/api/v1/events?limit=100", nil, &evs)
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Type] = true
	}
	for _, typ := range []string{"cert.renewal_requested", "cert.csr_ready", "cert.uploaded", "cert.installed"} {
		if !seen[typ] {
			t.Errorf("no %s event", typ)
		}
	}
}
