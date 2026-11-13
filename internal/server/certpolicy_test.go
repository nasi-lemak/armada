package server

import (
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
)

func TestCertRenewalPolicies(t *testing.T) {
	host, port, mails := fakeSMTP(t)
	s, ts := setupWith(t, func(c *Config) { c.SMTPRelay = api.EmailConfig{Host: host, Port: port, From: "pki@vendor.example"} })
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	ca := newAuthority(t, "Acme CA", nil)

	// acme-1's certificate expires in 40 days, acme-2's in 20, globex-1's in 10.
	type dev struct {
		id, key string
		cert    *x509.Certificate
	}
	devs := map[string]*dev{}
	for name, days := range map[string]int{"acme-1": 40, "acme-2": 20, "globex-1": 10} {
		customer := strings.Split(name, "-")[0]
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		key, _ := certs.GenerateKey("ecdsa-p256")
		devs[name] = &dev{er.DeviceID, er.DeviceKey, ca.issue(t, key.Public(), name+".example", []string{name + ".example"}, nil, clock.Add(-300*24*time.Hour), clock.Add(time.Duration(days)*24*time.Hour))}
	}
	// checkIn reports the device's certificate and answers a CSR request
	// (ok) or fails it; it returns the CSR made, if any.
	checkIn := func(d *dev, csrOK bool) string {
		t.Helper()
		info := certs.Describe(d.cert)
		info.KeyMatches = true
		var hr api.HeartbeatResponse
		call(t, ts, d.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}, Certs: []api.CertReport{{Target: "mosquitto", Info: &info}}}, &hr)
		csr := ""
		for _, a := range hr.Actions {
			if a.Type != api.ActionCertCSR {
				continue
			}
			var spec api.CertCSRSpec
			json.Unmarshal([]byte(a.Args[0]), &spec)
			res := api.ActionResult{Status: api.ActionFailed, Output: "disk full"}
			if csrOK {
				key, _ := certs.GenerateKey(spec.KeyType)
				b, _ := certs.MakeCSR(key, spec.Subject, spec.DNSNames, spec.IPAddresses)
				csr = string(b)
				res = api.ActionResult{Status: api.ActionSucceeded, Output: csr}
			}
			call(t, ts, d.key, "POST", "/api/v1/agent/actions/"+a.ID+"/result", res, nil)
		}
		return csr
	}
	for _, d := range devs {
		checkIn(d, true)
	}
	tick := func(after time.Duration) {
		t.Helper()
		clock = clock.Add(after)
		if err := s.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	mail := func() string {
		t.Helper()
		select {
		case m := <-mails:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no email")
		}
		return ""
	}
	noMail := func() {
		t.Helper()
		select {
		case m := <-mails:
			t.Fatalf("unexpected email: %.300s", m)
		case <-time.After(200 * time.Millisecond):
		}
	}
	renewal := func(d *dev) *api.CertRenewal {
		t.Helper()
		var det struct{ Certificates []api.DeviceCert }
		call(t, ts, admin, "GET", "/api/v1/devices/"+d.id, nil, &det)
		return det.Certificates[0].Renewal
	}

	// Who can set one up.
	// Sessions expire as the clock moves: sign in again where it has.
	signIn := func(email string) string {
		t.Helper()
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	login := func(email, role, customer string) string {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: role,
			Scope: api.Selector{Labels: map[string]string{"customer": customer}}, Password: "long-password-1"}, nil)
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	acmeIT, acmeCEO := login("it@acme.com", api.RoleOperator, "acme"), login("ceo@acme.com", api.RoleViewer, "acme")
	for name, c := range map[string]struct {
		token string
		pol   api.CertPolicy
		want  int
	}{
		"viewer":        {acmeCEO, api.CertPolicy{}, 403},
		"other client":  {acmeIT, api.CertPolicy{ClientScope: api.Selector{Labels: map[string]string{"customer": "globex"}}}, 403},
		"renew days":    {acmeIT, api.CertPolicy{RenewDays: 400}, 400},
		"reminder days": {acmeIT, api.CertPolicy{ReminderDays: -1}, 400},
		"key type":      {acmeIT, api.CertPolicy{KeyType: "dsa"}, 400},
		"recipient":     {acmeIT, api.CertPolicy{Recipients: []string{"nope"}}, 400},
		"target":        {acmeIT, api.CertPolicy{Targets: []string{"../etc"}}, 400},
	} {
		if code := call(t, ts, c.token, "POST", "/api/v1/cert-policies", c.pol, nil); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	var pol api.CertPolicy
	if code := call(t, ts, acmeIT, "POST", "/api/v1/cert-policies", api.CertPolicy{RenewDays: 30, ReminderDays: 7,
		Recipients: []string{"PKI Team <pki@acme.com>"}, Note: "Contract 77"}, &pol); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if !pol.Enabled || pol.ClientScope.Labels["customer"] != "acme" || pol.Name != "Renew customer=acme certificates" || pol.Recipients[0] != "pki@acme.com" {
		t.Fatalf("policy: %+v", pol)
	}

	// First pass: only acme-2 is within 30 days (acme-1 isn't yet; globex isn't acme's).
	tick(0)
	if r := renewal(devs["acme-2"]); r == nil || r.Status != api.RenewalRequested || r.PolicyID != pol.ID || r.RequestedBy != "renewal policy "+pol.Name {
		t.Fatalf("acme-2 renewal: %+v", r)
	}
	if renewal(devs["acme-1"]) != nil || renewal(devs["globex-1"]) != nil {
		t.Fatal("renewed a certificate outside the policy")
	}
	noMail() // nothing to send until the device has made its CSR

	// The device makes the CSR; the next pass emails it.
	csr2 := checkIn(devs["acme-2"], true)
	tick(5 * time.Minute)
	noMail() // passes run every 15 minutes
	tick(11 * time.Minute)
	m := mail()
	if !strings.Contains(m, "To: pki@acme.com") || !strings.Contains(m, "Subject: [Armada] 1 certificate signing request, the first expiring") || !strings.Contains(m, "application/zip") {
		t.Fatalf("email: %.800s", m)
	}
	if r := renewal(devs["acme-2"]); r.EmailedAt == nil || r.EmailedTo[0] != "pki@acme.com" {
		t.Fatalf("not marked emailed: %+v", r)
	}
	tick(16 * time.Minute)
	noMail() // sent once

	// Still not signed a week later: a reminder.
	tick(7 * 24 * time.Hour)
	if m := mail(); !strings.Contains(m, "Reminder") {
		t.Fatalf("reminder: %.400s", m)
	}
	tick(16 * time.Minute)
	noMail()

	// Ten days in, acme-1 is within 30 days too; its device fails to make
	// the CSR. That isn't retried for a week.
	tick(3 * 24 * time.Hour)
	if r := renewal(devs["acme-1"]); r == nil || r.Status != api.RenewalRequested {
		t.Fatalf("acme-1 renewal: %+v", r)
	}
	checkIn(devs["acme-1"], false)
	if r := renewal(devs["acme-1"]); r.Status != api.RenewalFailed {
		t.Fatalf("acme-1 after failure: %+v", r)
	}
	tick(16 * time.Minute)
	if r := renewal(devs["acme-1"]); r.Status != api.RenewalFailed {
		t.Fatal("failed renewal retried straight away")
	}
	tick(7 * 24 * time.Hour)
	if r := renewal(devs["acme-1"]); r.Status != api.RenewalRequested {
		t.Fatalf("failed renewal not retried after a week: %+v", r)
	}
	for len(mails) > 0 { // the weekly pass may also have reminded about acme-2
		<-mails
	}

	// acme-2's certificate comes back and is installed: nothing more to do for it.
	key2, _ := certs.ParseCSR([]byte(csr2))
	signed := ca.issue(t, key2.PublicKey, "acme-2.example", []string{"acme-2.example"}, nil, clock.Add(-time.Minute), clock.Add(397*24*time.Hour))
	acmeIT, acmeCEO = signIn("it@acme.com"), signIn("ceo@acme.com")
	var up api.UploadCertsResponse
	call(t, ts, acmeIT, "POST", "/api/v1/certificates/upload", api.UploadCertsRequest{PEM: string(certs.EncodeCerts(signed))}, &up)
	if len(up.Installing) != 1 {
		t.Fatalf("upload: %+v", up)
	}
	devs["acme-2"].cert = signed
	checkIn(devs["acme-2"], true) // reports the new certificate: installed
	if r := renewal(devs["acme-2"]); r.Status != api.RenewalInstalled {
		t.Fatalf("acme-2 installed: %+v", r)
	}
	tick(16 * time.Minute)
	if r := renewal(devs["acme-2"]); r.Status != api.RenewalInstalled {
		t.Fatal("renewed a fresh certificate")
	}

	// Disabled: no new renewals. Run now: a pass on demand.
	pol.Enabled = false
	if code := call(t, ts, acmeIT, "PUT", "/api/v1/cert-policies/"+pol.ID, pol, &pol); code != 200 || pol.Enabled {
		t.Fatalf("disable: %d", code)
	}
	call(t, ts, admin, "POST", "/api/v1/certificates/"+devs["acme-1"].id+"/mosquitto/cancel", nil, nil)
	tick(8 * 24 * time.Hour)
	if r := renewal(devs["acme-1"]); r.Status != api.RenewalCancelled {
		t.Fatalf("disabled policy renewed: %+v", r)
	}
	acmeIT, acmeCEO = signIn("it@acme.com"), signIn("ceo@acme.com")
	var res certPolicyResult
	if code := call(t, ts, acmeIT, "POST", "/api/v1/cert-policies/"+pol.ID+"/run", nil, &res); code != 200 || res.Started != 1 {
		t.Fatalf("run now: %d %+v", code, res)
	}
	if code := call(t, ts, acmeCEO, "POST", "/api/v1/cert-policies/"+pol.ID+"/run", nil, nil); code != 403 {
		t.Fatalf("viewer ran it: %d", code)
	}
	var list []api.CertPolicy
	call(t, ts, acmeCEO, "GET", "/api/v1/cert-policies", nil, &list)
	if len(list) != 1 || list[0].LastRunAt == nil {
		t.Fatalf("list: %+v", list)
	}
	if code := call(t, ts, acmeIT, "DELETE", "/api/v1/cert-policies/"+pol.ID, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}
