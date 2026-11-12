package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestRolloutNeedsSecondPerson(t *testing.T) {
	s, ts := setupWith(t, func(c *Config) { c.RolloutApproval = "second-person" })
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	for _, e := range []string{"ana@vendor.com", "ben@vendor.com"} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: e, Role: api.RoleOperator, Password: "operator-password"}, nil)
	}
	login := func(e string) string {
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: e, Password: "operator-password"}, &lr)
		return lr.Token
	}
	ana, ben := login("ana@vendor.com"), login("ben@vendor.com")
	rel := upload(t, ts, "app", "2.0.0")
	var d api.Deployment
	call(t, ts, ana, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID, Strategy: &api.Strategy{
		Stages: []int{5, 100}, AutoAdvance: true, MaxFailureRate: 0.5}}, &d)
	if d.Status != api.DeploymentRunning || !d.NeedsApproval || d.CreatedBy != "ana@vendor.com" {
		t.Fatalf("created: %+v", d)
	}
	// The canary runs, but the engine won't go further without approval.
	clock = clock.Add(time.Hour)
	s.Tick(context.Background())
	call(t, ts, ana, "GET", "/api/v1/deployments/"+d.ID, nil, &struct{ Deployment *api.Deployment }{&d})
	if d.StageIndex != 0 || d.StatusReason == "" {
		t.Fatalf("advanced without approval: %+v", d)
	}
	if c := call(t, ts, ana, "POST", "/api/v1/deployments/"+d.ID+"/advance", nil, nil); c != 409 {
		t.Fatalf("manual advance without approval: %d", c)
	}
	if c := call(t, ts, ana, "POST", "/api/v1/deployments/"+d.ID+"/approve", nil, nil); c != 403 {
		t.Fatalf("creator approved own rollout: %d", c)
	}
	var ap api.Deployment
	if c := call(t, ts, ben, "POST", "/api/v1/deployments/"+d.ID+"/approve", nil, &ap); c != 200 || ap.ApprovedBy != "ben@vendor.com" || ap.NeedsApproval || ap.StatusReason != "" {
		t.Fatalf("second person approve: %d %+v", c, ap)
	}
	s.Tick(context.Background())
	call(t, ts, ana, "GET", "/api/v1/deployments/"+d.ID, nil, &struct{ Deployment *api.Deployment }{&d})
	if d.StageIndex != 1 {
		t.Fatalf("not advanced after approval: %+v", d)
	}

	// A rollout whose first stage is already large waits before offering anything.
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Info: api.DeviceInfo{OS: "linux", Arch: "amd64"}}, &er)
	rel3 := upload(t, ts, "tool", "3.0.0")
	var big api.Deployment
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel3.ID, Strategy: &api.Strategy{Stages: []int{100}}}, &big)
	if big.Status != api.DeploymentAwaitingApproval {
		t.Fatalf("100%% first stage: %s", big.Status)
	}
	var hr api.HeartbeatResponse
	call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Arch: "amd64"}, Packages: map[string]string{"tool": "1.0", "app": "2.0.0"}}, &hr)
	if hr.Update != nil {
		t.Fatalf("offered before approval: %+v", hr.Update)
	}
	// The admin token is one identity: it can't approve its own rollout.
	if c := call(t, ts, admin, "POST", "/api/v1/deployments/"+big.ID+"/approve", nil, nil); c != 403 {
		t.Fatalf("admin token approved itself: %d", c)
	}
	call(t, ts, ana, "POST", "/api/v1/deployments/"+big.ID+"/approve", nil, nil)
	call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Arch: "amd64"}, Packages: map[string]string{"tool": "1.0", "app": "2.0.0"}}, &hr)
	if hr.Update == nil || hr.Update.Package != "tool" {
		t.Fatal("not offered after approval")
	}
}

func TestExecWipeAndCommandChecksNeedFleetPowers(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Info: api.DeviceInfo{OS: "linux"}}, &er)
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator, Scope: acme, Password: "client-password"}, nil)
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "op@vendor.com", Role: api.RoleOperator, Password: "operator-password"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "client-password"}, &lr)
	client := lr.Token
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "op@vendor.com", Password: "operator-password"}, &lr)
	op := lr.Token
	path := "/api/v1/devices/" + er.DeviceID + "/actions"
	if c := call(t, ts, client, "POST", path, api.CreateActionRequest{Type: api.ActionExec, Args: []string{"id"}}, nil); c != 403 {
		t.Fatalf("client operator exec: %d", c)
	}
	if c := call(t, ts, client, "POST", path, api.CreateActionRequest{Type: api.ActionLogs, Args: []string{"journal"}}, nil); c != 201 {
		t.Fatalf("client operator logs: %d", c)
	}
	if c := call(t, ts, op, "POST", path, api.CreateActionRequest{Type: api.ActionExec, Args: []string{"id"}}, nil); c != 201 {
		t.Fatalf("fleet operator exec: %d", c)
	}
	cmd := map[string]any{"name": "queue", "enabled": true, "check": map[string]any{"type": "command", "command": []string{"/usr/lib/nagios/plugins/check_load"}}}
	if c := call(t, ts, op, "POST", "/api/v1/monitors", cmd, nil); c != 403 {
		t.Fatalf("operator created a command check: %d", c)
	}
	var m api.Monitor
	if c := call(t, ts, admin, "POST", "/api/v1/monitors", cmd, &m); c != 201 {
		t.Fatalf("admin command check: %d", c)
	}
	// An operator can't turn a command check into something else, or back.
	if c := call(t, ts, op, "PUT", "/api/v1/monitors/"+m.ID, map[string]any{"name": "queue", "check": map[string]any{"type": "tcp", "target": "a:1"}}, nil); c != 403 {
		t.Fatalf("operator edited a command check: %d", c)
	}
	if c := call(t, ts, op, "POST", "/api/v1/monitors", map[string]any{"name": "web", "check": map[string]any{"type": "http", "target": "http://127.0.0.1"}}, nil); c != 201 {
		t.Fatalf("operator http check: %d", c)
	}
}

func TestTrustedProxiesAndLockouts(t *testing.T) {
	proxies, err := ParseTrustedProxies("127.0.0.1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	s, ts := setupWith(t, func(c *Config) { c.TrustedProxies = proxies })
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "dana@vendor.com", Role: api.RoleViewer, Password: "correct horse battery"}, nil)
	login := func(pw, from string) int {
		req := httptestRequest(ts.URL+"/api/v1/auth/login", api.LoginRequest{Email: "dana@vendor.com", Password: pw})
		if from != "" {
			req.Header.Set("X-Forwarded-For", from)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// An attacker at 203.0.113.9 (behind the trusted proxy) fails five times...
	for i := 0; i < 5; i++ {
		login("guess", "203.0.113.9")
	}
	if c := login("correct horse battery", "203.0.113.9"); c != 429 {
		t.Fatalf("attacker's address not throttled: %d", c)
	}
	// ...but Dana, from her own address, can still sign in.
	if c := login("correct horse battery", "198.51.100.7"); c != 200 {
		t.Fatalf("victim locked out by a stranger: %d", c)
	}
	// Spoofed hops left of the real client are ignored: the right-most
	// untrusted address counts.
	if c := login("correct horse battery", "198.51.100.7, 203.0.113.9"); c != 429 {
		t.Fatalf("spoofed X-Forwarded-For bypassed throttling: %d", c)
	}
	// Distributed guessing still hits the per-email cap.
	for i := 0; i < 60; i++ {
		login("guess", "192.0.2."+itoa(i))
	}
	if c := login("correct horse battery", "198.51.100.8"); c != 429 {
		t.Fatalf("no per-email cap: %d", c)
	}
	_ = s
}

func TestForwardedHeadersOnlyFromTrustedProxies(t *testing.T) {
	s, ts := setup(t) // no trusted proxies
	req := httptestRequest(ts.URL+"/api/v1/auth/login", api.LoginRequest{Email: "x@y.com", Password: "nope-nope-nope"})
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	r := req.Clone(context.Background())
	r.RemoteAddr = "192.0.2.10:5555"
	if got := s.clientIP(r); got != "192.0.2.10" {
		t.Fatalf("client IP from an untrusted header: %s", got)
	}
	if got := s.publicURL(r); got != "http://"+r.Host {
		t.Fatalf("public URL from an untrusted header: %s", got)
	}
}

var httpClient = &http.Client{}

func httptestRequest(url string, body any) *http.Request {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestAdminTokenCanBeDisabledSafely(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir, cfg.AdminTokens, cfg.AdminTokenDisabled = dir, nil, true
	cfg.Logger = setupLogger()
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "admin user") {
		t.Fatalf("disabled the admin token with no admin user: %v", err)
	}
	// With an admin user it starts, and the old token no longer works.
	s, ts := setupWith(t, func(c *Config) { c.DataDir = dir; c.AdminTokenDisabled = false })
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "root@vendor.com", Role: api.RoleAdmin, Password: "admin-password"}, nil)
	var evs []api.Event
	call(t, ts, admin, "GET", "/api/v1/events", nil, &evs)
	found := false
	for _, e := range evs {
		found = found || e.Type == "auth.admin_token"
	}
	if !found {
		t.Fatal("admin token use not recorded")
	}
	ts.Close()
	s.Close()
	_, ts2 := setupWith(t, func(c *Config) { c.DataDir = dir; c.AdminTokenDisabled = true })
	if c := call(t, ts2, admin, "GET", "/api/v1/devices", nil, nil); c != 401 {
		t.Fatalf("disabled admin token still works: %d", c)
	}
	var lr api.LoginResponse
	if c := call(t, ts2, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "root@vendor.com", Password: "admin-password"}, &lr); c != 200 {
		t.Fatalf("admin user sign-in: %d", c)
	}
}

func TestPasskeysAndSSONeedPublicURL(t *testing.T) {
	_, ts := setup(t)
	var ac api.AuthConfig
	call(t, ts, "", "GET", "/api/v1/auth/config", nil, &ac)
	if ac.Passkeys {
		t.Fatal("passkeys offered without -public-url")
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/passkey/begin", nil, nil); c != 400 {
		t.Fatalf("passkey sign-in without -public-url: %d", c)
	}
}

func setupLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
