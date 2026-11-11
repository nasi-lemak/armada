package server

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

type received struct {
	header http.Header
	body   []byte
}

func captureServer(t *testing.T) (*httptest.Server, chan received) {
	t.Helper()
	ch := make(chan received, 50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- received{r.Header.Clone(), b}
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func next(t *testing.T, ch chan received) received {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no notification received")
	}
	return received{}
}

func none(t *testing.T, ch chan received) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected notification: %s", r.body)
	case <-time.After(300 * time.Millisecond):
	}
}

func notification(t *testing.T, r received) api.AlertNotification {
	t.Helper()
	var n api.AlertNotification
	if err := json.Unmarshal(r.body, &n); err != nil {
		t.Fatalf("bad notification %s: %v", r.body, err)
	}
	return n
}

// alertEnv: a server, one enrolled device, a webhook channel and a clock.
type alertEnv struct {
	s     *Server
	ts    *httptest.Server
	hook  chan received
	chID  string
	devID string
	key   string
	clock time.Time
}

func setupAlerts(t *testing.T) *alertEnv {
	s, ts := setup(t)
	e := &alertEnv{s: s, ts: ts, clock: time.Now().UTC()}
	s.now = func() time.Time { return e.clock }
	hook, ch := captureServer(t)
	e.hook = ch
	var c api.NotificationChannel
	if code := call(t, ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "ops", Type: "webhook",
		Webhook: &api.WebhookConfig{URL: hook.URL, Secret: "s3cret", Headers: map[string]string{"Authorization": "Bearer abc"}}}, &c); code != 201 {
		t.Fatalf("create channel: %d", code)
	}
	e.chID = c.ID
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"site": "sg"}}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "kiosk-07", Info: api.DeviceInfo{OS: "linux"}}, &er)
	e.devID, e.key = er.DeviceID, er.DeviceKey
	return e
}

func (e *alertEnv) rule(t *testing.T, r api.AlertRule) {
	t.Helper()
	r.Enabled = true
	r.Channels = []string{e.chID}
	if code := call(t, e.ts, admin, "POST", "/api/v1/alert-rules", r, nil); code != 201 {
		t.Fatalf("create rule: %d", code)
	}
}

func (e *alertEnv) tick(t *testing.T) {
	t.Helper()
	if err := e.s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (e *alertEnv) heartbeat(t *testing.T, checks []api.CheckResult) {
	t.Helper()
	call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}, Health: checks,
		Metrics: api.Metrics{NumCPU: 4, Load1: 0.42, MemTotalBytes: 4 << 30, MemAvailableBytes: 3 << 30}}, nil)
}

func TestWebhookChannelSigningTemplateAndRedaction(t *testing.T) {
	e := setupAlerts(t)
	if code := call(t, e.ts, admin, "POST", "/api/v1/channels/"+e.chID+"/test", nil, nil); code != 200 {
		t.Fatalf("test send: %d", code)
	}
	r := next(t, e.hook)
	ts, sig := r.header.Get("X-Armada-Timestamp"), r.header.Get("X-Armada-Signature")
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write([]byte(ts + "."))
	mac.Write(r.body)
	if sig != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatalf("bad signature %q", sig)
	}
	if r.header.Get("Authorization") != "Bearer abc" || notification(t, r).Status != "test" {
		t.Fatalf("headers/body: %v %s", r.header, r.body)
	}
	// Secrets never come back from the API, and saving the redacted form keeps them.
	var list []api.NotificationChannel
	call(t, e.ts, admin, "GET", "/api/v1/channels", nil, &list)
	if list[0].Webhook.Secret != redacted || list[0].Webhook.Headers["Authorization"] != redacted {
		t.Fatalf("secrets leaked: %+v", list[0].Webhook)
	}
	list[0].Webhook.BodyTemplate = `{"routing_key":"abc","event_action":"{{if eq .Status "resolved"}}resolve{{else}}trigger{{end}}","payload":{"summary":{{json .Summary}},"severity":"{{.Severity}}","source":"{{.DeviceName}}"}}`
	if code := call(t, e.ts, admin, "PUT", "/api/v1/channels/"+e.chID, list[0], nil); code != 200 {
		t.Fatalf("update: %d", code)
	}
	call(t, e.ts, admin, "POST", "/api/v1/channels/"+e.chID+"/test", nil, nil)
	r = next(t, e.hook)
	var pd map[string]any
	if err := json.Unmarshal(r.body, &pd); err != nil || pd["event_action"] != "trigger" || r.header.Get("Authorization") != "Bearer abc" {
		t.Fatalf("templated body %s (err %v), auth %q", r.body, err, r.header.Get("Authorization"))
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "bad", Type: "webhook",
		Webhook: &api.WebhookConfig{URL: "http://x", BodyTemplate: "{{.Nope}}"}}, nil); code != 400 {
		t.Fatalf("invalid template accepted: %d", code)
	}
}

func TestOfflineAlertFiresAndResolves(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Device offline", Condition: api.ConditionDeviceOffline, OfflineMinutes: 10,
		Selector: api.Selector{Labels: map[string]string{"site": "sg"}}, NotifyResolved: true, RepeatMinutes: 60})
	e.tick(t)
	none(t, e.hook)
	e.clock = e.clock.Add(11 * time.Minute)
	e.tick(t)
	n := notification(t, next(t, e.hook))
	if n.Status != api.AlertFiring || n.Severity != api.HealthCritical || n.DeviceName != "kiosk-07" || n.DeviceLabels["site"] != "sg" {
		t.Fatalf("firing: %+v", n)
	}
	e.tick(t)
	none(t, e.hook) // no repeat before the repeat interval
	e.clock = e.clock.Add(61 * time.Minute)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertFiring || !strings.Contains(n.Summary, "1.2 h") {
		t.Fatalf("repeat: %+v", n)
	}
	e.heartbeat(t, nil)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved || n.ResolvedAt == nil {
		t.Fatalf("resolved: %+v", n)
	}
	var alerts []api.Alert
	call(t, e.ts, admin, "GET", "/api/v1/alerts?status=firing", nil, &alerts)
	if len(alerts) != 0 {
		t.Fatalf("still firing: %+v", alerts)
	}
}

func TestHealthAlertEscalatesAndFiltersKinds(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Storage failing", Condition: api.ConditionHealth, MinSeverity: api.HealthWarning,
		CheckKinds: []string{"storage"}, NotifyResolved: true})
	hot := api.CheckResult{Name: "temperature cpu", Kind: "temperature", Status: api.HealthCritical, Message: "95°C"}
	e.heartbeat(t, []api.CheckResult{hot})
	e.tick(t)
	none(t, e.hook) // other kinds of check are ignored by this rule

	warn := api.CheckResult{Name: "io-errors /dev/mmcblk0", Kind: "storage", Status: api.HealthWarning, Message: "3 kernel I/O errors in 24h"}
	e.heartbeat(t, []api.CheckResult{hot, warn})
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Severity != api.HealthWarning || !strings.Contains(n.Summary, "mmcblk0") {
		t.Fatalf("warning: %+v", n)
	}
	crit := api.CheckResult{Name: "readonly /", Kind: "storage", Status: api.HealthCritical, Message: "root is read-only"}
	e.heartbeat(t, []api.CheckResult{hot, warn, crit})
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Severity != api.HealthCritical || !strings.Contains(n.Summary, "readonly /") {
		t.Fatalf("escalation: %+v", n)
	}
	e.heartbeat(t, []api.CheckResult{hot})
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved {
		t.Fatalf("resolve: %+v", n)
	}
}

func TestUpdateFailuresFoldIntoOneAlert(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Update failed", Condition: api.ConditionUpdateFailed})
	e.rule(t, api.AlertRule{Name: "Rollout paused", Condition: api.ConditionDeploymentPaused, NotifyResolved: true})
	for i := 0; i < 3; i++ {
		e.s.event(t.Context(), "update.failed", e.devID, "dep_bad", "health check failed")
	}
	n := notification(t, next(t, e.hook))
	if n.Rule != "Update failed" || n.DeploymentID != "dep_bad" {
		t.Fatalf("update failed: %+v", n)
	}
	none(t, e.hook) // later failures fold in
	var alerts []api.Alert
	call(t, e.ts, admin, "GET", "/api/v1/alerts?status=firing", nil, &alerts)
	if len(alerts) != 1 || alerts[0].Count != 3 {
		t.Fatalf("folded alert: %+v", alerts)
	}
	// Event alerts close after the folding window, silently.
	e.clock = e.clock.Add(2 * time.Hour)
	e.tick(t)
	none(t, e.hook)

	rel := upload(t, e.ts, "app", "1.0.0")
	var dep api.Deployment
	call(t, e.ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID}, &dep)
	call(t, e.ts, admin, "POST", "/api/v1/deployments/"+dep.ID+"/pause", nil, nil)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Rule != "Rollout paused" || n.DeploymentID != dep.ID {
		t.Fatalf("paused: %+v", n)
	}
	call(t, e.ts, admin, "POST", "/api/v1/deployments/"+dep.ID+"/resume", nil, nil)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved {
		t.Fatalf("resumed: %+v", n)
	}
}

func TestSlackAndEmailChannels(t *testing.T) {
	e := setupAlerts(t)
	slack, slackCh := captureServer(t)
	var sc api.NotificationChannel
	call(t, e.ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "slack", Type: "slack", Slack: &api.SlackConfig{WebhookURL: slack.URL + "/services/T/B/x"}}, &sc)
	if !strings.HasSuffix(sc.Slack.WebhookURL, "/"+redacted) {
		t.Fatalf("slack URL not redacted: %s", sc.Slack.WebhookURL)
	}
	call(t, e.ts, admin, "POST", "/api/v1/channels/"+sc.ID+"/test", nil, nil)
	var msg map[string]string
	json.Unmarshal(next(t, slackCh).body, &msg)
	if !strings.Contains(msg["text"], "TEST: Test notification") {
		t.Fatalf("slack text: %q", msg["text"])
	}

	// A minimal SMTP server that records the message.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	got := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
		say := func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
		say("220 test ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := rw.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					say("250 queued")
					got <- data.String()
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.Fields(line)[0]); cmd {
			case "EHLO", "HELO":
				say("250 test")
			case "DATA":
				inData = true
				say("354 go ahead")
			case "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	var ec api.NotificationChannel
	var p int
	json.Unmarshal([]byte(port), &p)
	if code := call(t, e.ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "mail", Type: "email",
		Email: &api.EmailConfig{Host: host, Port: p, From: "armada@example.com", To: []string{"ops@example.com"}}}, &ec); code != 201 {
		t.Fatalf("email channel: %d", code)
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/channels/"+ec.ID+"/test", nil, nil); code != 200 {
		t.Fatalf("email test: %d", code)
	}
	select {
	case m := <-got:
		if !strings.Contains(m, "Subject: =?utf-8?q?[Armada]_TEST:_Test_notification") || !strings.Contains(m, "This is a test notification") {
			t.Fatalf("email: %s", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no email")
	}
}
