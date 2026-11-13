package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestClientAlerts(t *testing.T) {
	s, ts := setup(t)
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }

	// The client's webhook receiver (HTTPS, as client webhooks require).
	got := make(chan api.AlertNotification, 10)
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n api.AlertNotification
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &n)
		got <- n
	}))
	defer hook.Close()
	s.allowPrivateClientTargets = true // the test receiver is on 127.0.0.1
	s.clientNotifyClient.Transport.(*http.Transport).TLSClientConfig = hook.Client().Transport.(*http.Transport).TLSClientConfig

	devs := map[string]string{}
	for _, c := range []string{"acme", "globex"} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: c + "-kiosk", Info: api.DeviceInfo{OS: "linux"}}, &er)
		call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, nil)
		devs[c] = er.DeviceID
	}
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	globex := api.Selector{Labels: map[string]string{"customer": "globex"}}
	users := map[string]string{}
	for email, g := range map[string]api.Grant{
		"it@acme.com": {Role: api.RoleOperator, Scope: acme}, "ceo@acme.com": {Role: api.RoleViewer, Scope: acme},
		"it@globex.com": {Role: api.RoleOperator, Scope: globex}, "ops@vendor.com": {Role: api.RoleOperator},
	} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: g.Role, Scope: g.Scope, Password: "long-password-1"}, nil)
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		users[email] = lr.Token
	}
	acmeIT, acmeCEO, globexIT, vendorOps := users["it@acme.com"], users["ceo@acme.com"], users["it@globex.com"], users["ops@vendor.com"]

	// Client channels: HTTPS only, no custom templates.
	for name, bad := range map[string]*api.WebhookConfig{
		"plain http":    {URL: "http://hooks.example.com/x"},
		"body template": {URL: hook.URL, BodyTemplate: "{{.Summary}}"},
	} {
		if c := call(t, ts, acmeIT, "POST", "/api/v1/channels", api.NotificationChannel{Name: "x", Type: "webhook", Webhook: bad}, nil); c != 400 {
			t.Errorf("client channel with %s: %d", name, c)
		}
	}
	var ch api.NotificationChannel
	if c := call(t, ts, acmeIT, "POST", "/api/v1/channels", api.NotificationChannel{Name: "Acme on-call", Type: "webhook",
		Webhook: &api.WebhookConfig{URL: hook.URL, Secret: "s3cret"}}, &ch); c != 201 || ch.ClientScope.Labels["customer"] != "acme" {
		t.Fatalf("client channel: %d %+v", c, ch)
	}
	// Email for clients goes through the vendor's relay only.
	if c := call(t, ts, acmeIT, "POST", "/api/v1/channels", api.NotificationChannel{Name: "mail", Type: "email",
		Email: &api.EmailConfig{To: []string{"ops@acme.com"}}}, nil); c != 400 {
		t.Fatalf("client email without relay: %d", c)
	}
	s.cfg.SMTPRelay = api.EmailConfig{Host: "smtp.vendor.example", Port: 587, From: "alerts@vendor.example"}
	var mailCh api.NotificationChannel
	if c := call(t, ts, acmeIT, "POST", "/api/v1/channels", api.NotificationChannel{Name: "mail", Type: "email",
		Email: &api.EmailConfig{Host: "10.0.0.5", Port: 25, From: "x@acme.com", To: []string{"ops@acme.com"}}}, &mailCh); c != 201 || mailCh.Email.Host != "" {
		t.Fatalf("client email channel kept its own server: %d %+v", c, mailCh.Email)
	}

	// Vendor and client channels don't mix.
	var vendorCh api.NotificationChannel
	call(t, ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "Vendor NOC", Type: "webhook", Webhook: &api.WebhookConfig{URL: "http://noc.internal/hook"}}, &vendorCh)
	if c := call(t, ts, acmeIT, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Enabled: true, Condition: api.ConditionHealth, Channels: []string{vendorCh.ID}}, nil); c != 400 {
		t.Fatalf("client rule on vendor channel: %d", c)
	}
	if c := call(t, ts, admin, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Enabled: true, Condition: api.ConditionHealth, Channels: []string{ch.ID}}, nil); c != 400 {
		t.Fatalf("vendor rule on client channel: %d", c)
	}
	if c := call(t, ts, acmeIT, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Enabled: true, Condition: api.ConditionUpdateFailed, Channels: []string{ch.ID}}, nil); c != 400 {
		t.Fatalf("client update_failed rule: %d", c)
	}
	// Fleet-wide alerts stay admin-only, even for fleet operators.
	if c := call(t, ts, vendorOps, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Enabled: true, Condition: api.ConditionHealth}, nil); c != 403 {
		t.Fatalf("fleet operator created a vendor rule: %d", c)
	}

	// The client's rule: offline for 5 minutes.
	var rule api.AlertRule
	if c := call(t, ts, acmeIT, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "Kiosk offline", Enabled: true,
		Condition: api.ConditionDeviceOffline, OfflineMinutes: 5, Channels: []string{ch.ID}, NotifyResolved: true}, &rule); c != 201 {
		t.Fatalf("client rule: %d", c)
	}
	if rule.CreatedBy != "it@acme.com" || rule.ClientScope.Labels["customer"] != "acme" {
		t.Fatalf("rule: %+v", rule)
	}
	// Who sees what.
	listRules := func(tok string) []api.AlertRule {
		var rs []api.AlertRule
		call(t, ts, tok, "GET", "/api/v1/alert-rules", nil, &rs)
		return rs
	}
	if len(listRules(acmeCEO)) != 1 || len(listRules(globexIT)) != 0 || len(listRules(admin)) != 1 {
		t.Fatal("rule visibility wrong")
	}
	if c := call(t, ts, acmeCEO, "PUT", "/api/v1/alert-rules/"+rule.ID, rule, nil); c != 403 {
		t.Fatalf("client viewer edited a rule: %d", c)
	}
	if c := call(t, ts, globexIT, "DELETE", "/api/v1/alert-rules/"+rule.ID, nil, nil); c != 404 {
		t.Fatalf("other client touched the rule: %d", c)
	}
	var chs []api.NotificationChannel
	call(t, ts, globexIT, "GET", "/api/v1/channels", nil, &chs)
	if len(chs) != 0 {
		t.Fatalf("other client sees channels: %+v", chs)
	}
	call(t, ts, acmeCEO, "GET", "/api/v1/channels", nil, &chs)
	if len(chs) != 2 || chs[0].Webhook != nil && chs[0].Webhook.Secret != redacted {
		t.Fatalf("client viewer's channels: %+v", chs)
	}

	// Both devices go silent: only Acme's fires the client rule, and the
	// notification is about Acme's device only.
	clock = clock.Add(10 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n.DeviceID != devs["acme"] || n.Rule != "Kiosk offline" || n.DeviceLabels["customer"] != "acme" {
			t.Fatalf("notification: %+v", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client webhook not called")
	}
	select {
	case n := <-got:
		t.Fatalf("second notification (other client's device?): %+v", n)
	case <-time.After(500 * time.Millisecond):
	}
	var alerts []api.Alert
	call(t, ts, globexIT, "GET", "/api/v1/alerts", nil, &alerts)
	for _, a := range alerts {
		if a.RuleID == rule.ID {
			t.Fatal("other client sees the client's alert")
		}
	}
}

func TestClientWebhooksCannotReachPrivateNetworks(t *testing.T) {
	s, ts := setup(t)
	internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("client webhook reached an internal address")
	}))
	defer internal.Close()
	s.clientNotifyClient.Transport.(*http.Transport).TLSClientConfig = internal.Client().Transport.(*http.Transport).TLSClientConfig
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator, Scope: acme, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	for _, u := range []string{"https://127.0.0.1/x", "https://10.1.2.3/x", "https://[::1]/x", "https://169.254.169.254/latest/meta-data"} {
		if c := call(t, ts, lr.Token, "POST", "/api/v1/channels", api.NotificationChannel{Name: "x", Type: "webhook", Webhook: &api.WebhookConfig{URL: u}}, nil); c != 400 {
			t.Errorf("client channel to %s: %d", u, c)
		}
	}
	// A host name that resolves to a private address passes the form check
	// but is refused when connecting.
	port := internal.URL[strings.LastIndex(internal.URL, ":"):]
	var ch api.NotificationChannel
	if c := call(t, ts, lr.Token, "POST", "/api/v1/channels", api.NotificationChannel{Name: "sneaky", Type: "webhook", Webhook: &api.WebhookConfig{URL: "https://localhost" + port + "/x"}}, &ch); c != 201 {
		t.Fatalf("create: %d", c)
	}
	if c := call(t, ts, lr.Token, "POST", "/api/v1/channels/"+ch.ID+"/test", nil, nil); c != 502 {
		t.Fatalf("test send to a private address: %d", c)
	}
}

func TestPublicIPClassification(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true, "::ffff:8.8.8.8": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.20.1.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fd00::1": false, "::ffff:10.0.0.1": false,
		"::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
