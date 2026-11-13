package server

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

type certAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newAuthority(t *testing.T, name string, parent *certAuthority) *certAuthority {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(5 * 365 * 24 * time.Hour)}
	signer, signKey := tpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &certAuthority{c, key}
}

// issue signs a certificate for a public key.
func (ca *certAuthority) issue(t *testing.T, pub any, cn string, dns []string, ips []string, from, to time.Time) *x509.Certificate {
	t.Helper()
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn, Organization: []string{"Acme"}}, DNSNames: dns,
		NotBefore: from, NotAfter: to, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, ip := range ips {
		tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestCertificateRenewals(t *testing.T) {
	host, port, mails := fakeSMTP(t)
	s, ts := setupWith(t, func(c *Config) {
		c.SMTPRelay = api.EmailConfig{Host: host, Port: port, From: "pki@vendor.example"}
		c.Android.PublicURL = "https://fleet.example"
	})
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	root := newAuthority(t, "Acme Root", nil)
	inter := newAuthority(t, "Acme Issuing CA", root)

	// Two devices, one per client, each with a Mosquitto certificate expiring in 20 days.
	type dev struct{ id, key string }
	devs := map[string]dev{}
	for _, c := range []string{"acme", "globex"} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: c + "-broker", Info: api.DeviceInfo{OS: "linux", IPs: []string{"10.0.0.7"}}}, &er)
		devs[c] = dev{er.DeviceID, er.DeviceKey}
	}
	acme, globex := devs["acme"], devs["globex"]
	oldKey, _ := certs.GenerateKey("ecdsa-p256")
	oldCert := inter.issue(t, oldKey.Public(), "broker.acme.example", []string{"broker.acme.example"}, []string{"10.0.0.7"}, clock.Add(-340*24*time.Hour), clock.Add(20*24*time.Hour))
	report := func(d dev, c *x509.Certificate) api.HeartbeatResponse {
		t.Helper()
		info := certs.Describe(c)
		info.KeyMatches = true
		var hr api.HeartbeatResponse
		call(t, ts, d.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", IPs: []string{"10.0.0.7"}},
			Certs: []api.CertReport{{Target: "mosquitto", Info: &info}}}, &hr)
		return hr
	}
	report(acme, oldCert)
	report(globex, oldCert)

	// Client accounts.
	users := map[string]string{}
	for email, g := range map[string]api.Grant{
		"it@acme.com":   {Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}},
		"ceo@acme.com":  {Role: api.RoleViewer, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}},
		"it@globex.com": {Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "globex"}}},
	} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: g.Role, Scope: g.Scope, Password: "long-password-1"}, nil)
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		users[email] = lr.Token
	}
	acmeIT, acmeCEO, globexIT := users["it@acme.com"], users["ceo@acme.com"], users["it@globex.com"]

	var list []api.DeviceCert
	call(t, ts, acmeCEO, "GET", "/api/v1/certificates", nil, &list)
	if len(list) != 1 || list[0].DeviceID != acme.id || list[0].DaysLeft == nil || *list[0].DaysLeft != 19 {
		t.Fatalf("client inventory: %+v", list)
	}
	call(t, ts, admin, "GET", "/api/v1/certificates?expiring_within=30", nil, &list)
	if len(list) != 2 {
		t.Fatalf("expiring: %d", len(list))
	}

	// Expiry alert for the client.
	var ch api.NotificationChannel
	hook, hooks := captureServer(t)
	call(t, ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "ops", Type: "webhook", Webhook: &api.WebhookConfig{URL: hook.URL}}, &ch)
	if code := call(t, ts, admin, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "Certificates", Enabled: true, Condition: api.ConditionCertExpiry,
		ExpiryDays: 30, Channels: []string{ch.ID}, NotifyResolved: true}, nil); code != 201 {
		t.Fatalf("rule: %d", code)
	}
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if n := notification(t, next(t, hooks)); n.Severity != api.HealthWarning || !strings.Contains(n.Summary, "mosquitto certificate (broker.acme.example) expires in 19 days") {
			t.Fatalf("expiry alert: %+v", n)
		}
	}

	// Renewals: viewers can't; clients only for their devices.
	if code := call(t, ts, acmeCEO, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true}, nil); code != 403 {
		t.Fatalf("viewer renewed: %d", code)
	}
	if code := call(t, ts, acmeIT, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{Selector: api.Selector{DeviceIDs: []string{globex.id}}}, nil); code != 404 {
		t.Fatalf("client renewed another client's: %d", code)
	}
	if code := call(t, ts, acmeIT, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true, KeyType: "dsa"}, nil); code != 400 {
		t.Fatalf("bad key type: %d", code)
	}
	var rr api.RenewCertsResponse
	call(t, ts, acmeIT, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true, KeyType: "rsa-2048",
		DNSNames: []string{"{{.Name}}.acme.example", "broker.acme.example"}}, &rr)
	if len(rr.Started) != 1 || rr.Started[0].Renewal.Status != api.RenewalRequested {
		t.Fatalf("renew: %+v", rr)
	}
	spec := rr.Started[0].Renewal.Spec
	if spec.Subject.CommonName != "broker.acme.example" || spec.Subject.Organization != "Acme" || spec.KeyType != "rsa-2048" ||
		strings.Join(spec.DNSNames, ",") != "acme-broker.acme.example,broker.acme.example" || spec.IPAddresses[0] != "10.0.0.7" {
		t.Fatalf("spec: %+v", spec)
	}
	call(t, ts, acmeIT, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true}, &rr)
	if len(rr.Started) != 0 || len(rr.Skipped) != 1 || !strings.Contains(rr.Skipped[0].Reason, "already in progress") {
		t.Fatalf("second renew: %+v", rr)
	}

	// The device makes the CSR.
	hr := report(acme, oldCert)
	if len(hr.Actions) != 1 || hr.Actions[0].Type != api.ActionCertCSR {
		t.Fatalf("csr action: %+v", hr.Actions)
	}
	var gotSpec api.CertCSRSpec
	json.Unmarshal([]byte(hr.Actions[0].Args[0]), &gotSpec)
	newKey, _ := certs.GenerateKey(gotSpec.KeyType)
	csrPEM, _ := certs.MakeCSR(newKey, gotSpec.Subject, gotSpec.DNSNames, gotSpec.IPAddresses)
	call(t, ts, acme.key, "POST", "/api/v1/agent/actions/"+hr.Actions[0].ID+"/result", api.ActionResult{Status: api.ActionSucceeded, Output: string(csrPEM)}, nil)
	call(t, ts, acmeCEO, "GET", "/api/v1/certificates?renewal=csr_ready", nil, &list)
	if len(list) != 1 || list[0].Renewal.CSR == "" {
		t.Fatalf("csr ready: %+v", list)
	}

	// CSRs out: a zip with a manifest, or by email.
	req, _ := httpGet(t, ts.URL+"/api/v1/certificates/csrs", acmeCEO)
	zr, err := zip.NewReader(bytes.NewReader(req), int64(len(req)))
	if err != nil || len(zr.File) != 2 || zr.File[0].Name != "acme-broker_mosquitto.csr" || zr.File[1].Name != "manifest.csv" {
		t.Fatalf("zip: %v", err)
	}
	if code := call(t, ts, acmeCEO, "POST", "/api/v1/certificates/email", api.EmailCSRsRequest{All: true, Recipients: []string{"pki@acme.com"}}, nil); code != 403 {
		t.Fatalf("viewer emailed: %d", code)
	}
	if code := call(t, ts, acmeIT, "POST", "/api/v1/certificates/email", api.EmailCSRsRequest{All: true, Recipients: []string{"PKI <pki@acme.com>"}, Note: "Ticket 42"}, nil); code != 200 {
		t.Fatalf("email: %d", code)
	}
	select {
	case m := <-mails:
		if !strings.Contains(m, "To: pki@acme.com") || !strings.Contains(m, `filename="csrs-`) || !strings.Contains(m, "application/zip") {
			t.Fatalf("email: %.600s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no email")
	}

	// Signed certificates back. The wrong ones are refused with reasons.
	stranger, _ := certs.GenerateKey("ecdsa-p256")
	good := inter.issue(t, newKey.Public(), "broker.acme.example", spec.DNSNames, spec.IPAddresses, clock.Add(-time.Minute), clock.Add(397*24*time.Hour))
	for name, c := range map[string]struct {
		pem    []byte
		reason string
	}{
		"other key":     {certs.EncodeCerts(inter.issue(t, stranger.Public(), "x", nil, nil, clock, clock.Add(time.Hour))), "doesn't match any waiting CSR"},
		"missing name":  {certs.EncodeCerts(inter.issue(t, newKey.Public(), "broker.acme.example", []string{"broker.acme.example"}, spec.IPAddresses, clock, clock.Add(time.Hour))), "missing the DNS name acme-broker.acme.example"},
		"expired":       {certs.EncodeCerts(inter.issue(t, newKey.Public(), "broker.acme.example", spec.DNSNames, spec.IPAddresses, clock.Add(-48*time.Hour), clock.Add(-time.Hour))), "expired"},
		"another's CSR": {certs.EncodeCerts(good), "doesn't match any waiting CSR"}, // uploaded by globex
	} {
		token := acmeIT
		if name == "another's CSR" {
			token = globexIT
		}
		var up api.UploadCertsResponse
		call(t, ts, token, "POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: string(c.pem)}, &up)
		if len(up.Installing) != 0 || len(up.Rejected) != 1 || !strings.Contains(up.Rejected[0].Reason, c.reason) {
			t.Errorf("%s: %+v", name, up)
		}
	}
	if code := call(t, ts, acmeIT, "POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: "not a certificate"}, nil); code != 400 {
		t.Fatalf("garbage upload: %d", code)
	}
	// The right one, with its chain and root, in any order.
	var up api.UploadCertsResponse
	call(t, ts, acmeIT, "POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: string(certs.EncodeCerts(root.cert, good, inter.cert))}, &up)
	if len(up.Installing) != 1 || len(up.Rejected) != 0 {
		t.Fatalf("upload: %+v", up)
	}
	hr = report(acme, oldCert)
	if len(hr.Actions) != 1 || hr.Actions[0].Type != api.ActionCertInstall {
		t.Fatalf("install action: %+v", hr.Actions)
	}
	var inst api.CertInstallSpec
	json.Unmarshal([]byte(hr.Actions[0].Args[0]), &inst)
	chain, _ := certs.ParseCerts([]byte(inst.Chain))
	if len(chain) != 1 || chain[0].Subject.CommonName != "Acme Issuing CA" {
		t.Fatalf("chain sent (root left out): %d certificates", len(chain))
	}
	info := certs.Describe(good)
	info.KeyMatches = true
	out, _ := json.Marshal(info)
	call(t, ts, acme.key, "POST", "/api/v1/agent/actions/"+hr.Actions[0].ID+"/result", api.ActionResult{Status: api.ActionSucceeded, Output: string(out)}, nil)
	report(acme, good)
	call(t, ts, acmeCEO, "GET", "/api/v1/certificates", nil, &list)
	if list[0].Renewal.Status != api.RenewalInstalled || list[0].Installed.Fingerprint != info.Fingerprint || *list[0].DaysLeft < 390 {
		t.Fatalf("installed: %+v", list[0].Renewal)
	}
	call(t, ts, acmeIT, "POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: string(certs.EncodeCerts(good))}, &up)
	if len(up.Rejected) != 1 || !strings.Contains(up.Rejected[0].Reason, "already installed") {
		t.Fatalf("re-upload: %+v", up)
	}
	// The alert for acme resolves; globex's stays.
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := notification(t, next(t, hooks)); n.Status != api.AlertResolved || n.DeviceID != acme.id {
		t.Fatalf("resolved: %+v", n)
	}

	// A failed install is reported, and a waiting renewal can be cancelled.
	call(t, ts, globexIT, "POST", "/api/v1/certificates/renew", api.RenewCertsRequest{All: true}, &rr)
	if code := call(t, ts, acmeIT, "POST", "/api/v1/certificates/"+globex.id+"/mosquitto/cancel", nil, nil); code != 404 {
		t.Fatalf("client cancelled another's: %d", code)
	}
	if code := call(t, ts, globexIT, "POST", "/api/v1/certificates/"+globex.id+"/mosquitto/cancel", nil, nil); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	hr = report(globex, oldCert) // the CSR action is still delivered; its result is ignored
	for _, a := range hr.Actions {
		call(t, ts, globex.key, "POST", "/api/v1/agent/actions/"+a.ID+"/result", api.ActionResult{Status: api.ActionSucceeded, Output: string(csrPEM)}, nil)
	}
	call(t, ts, globexIT, "GET", "/api/v1/certificates", nil, &list)
	if list[0].Renewal.Status != api.RenewalCancelled {
		t.Fatalf("cancelled renewal revived: %+v", list[0].Renewal)
	}

	// Reports list the certificates.
	var rep api.Report
	call(t, ts, acmeCEO, "GET", "/api/v1/reports?period=this-month", nil, &rep)
	if len(rep.Certificates) != 1 || rep.Certificates[0].Target != "mosquitto" || rep.Summary.CertsExpiringSoon != 0 {
		t.Fatalf("report certificates: %+v", rep.Certificates)
	}

	// A device that stops managing the certificate drops it.
	call(t, ts, acme.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}, Certs: []api.CertReport{}}, nil)
	call(t, ts, acmeCEO, "GET", "/api/v1/certificates", nil, &list)
	if len(list) != 0 {
		t.Fatalf("dropped certificate still listed: %+v", list)
	}
}

func httpGet(t *testing.T, url, token string) ([]byte, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", url, res.StatusCode, b)
	}
	return b, res.StatusCode
}
