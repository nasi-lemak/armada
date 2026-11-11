package server

import (
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestUsersRolesAndClientScoping(t *testing.T) {
	_, ts := setup(t)
	// Two customers' devices.
	enroll := func(name, customer string) api.EnrollResponse {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er
	}
	acme1, acme2, globex := enroll("acme-1", "acme"), enroll("acme-2", "acme"), enroll("globex-1", "globex")

	// --- creating users ---
	mk := func(req api.CreateUserRequest) int { return call(t, ts, admin, "POST", "/api/v1/users", req, nil) }
	if code := mk(api.CreateUserRequest{Email: "ops@vendor.com", Name: "Ops", Role: api.RoleAdmin, Password: "correct horse battery"}); code != 201 {
		t.Fatalf("create admin: %d", code)
	}
	if code := mk(api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "acme-password-1"}); code != 201 {
		t.Fatalf("create scoped operator: %d", code)
	}
	if code := mk(api.CreateUserRequest{Email: "audit@vendor.com", Role: api.RoleViewer, Password: "viewer-password"}); code != 201 {
		t.Fatalf("create viewer: %d", code)
	}
	for name, bad := range map[string]api.CreateUserRequest{
		"scoped admin":    {Email: "x@y.com", Role: api.RoleAdmin, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long enough pw"},
		"short password":  {Email: "x@y.com", Role: api.RoleViewer, Password: "short"},
		"bad role":        {Email: "x@y.com", Role: "root", Password: "long enough pw"},
		"bad email":       {Email: "nope", Role: api.RoleViewer, Password: "long enough pw"},
		"device-id scope": {Email: "x@y.com", Role: api.RoleViewer, Scope: api.Selector{DeviceIDs: []string{acme1.DeviceID}}, Password: "long enough pw"},
		"duplicate":       {Email: "OPS@vendor.com", Role: api.RoleViewer, Password: "long enough pw"},
	} {
		if code := mk(bad); code != 400 && code != 409 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}

	// --- signing in ---
	login := func(email, pw string) (string, int) {
		var lr api.LoginResponse
		code := call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: pw}, &lr)
		return lr.Token, code
	}
	if _, code := login("it@acme.com", "wrong"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	acme, code := login("IT@acme.com", "acme-password-1")
	if code != 200 || !strings.HasPrefix(acme, "st_") {
		t.Fatalf("login: %d", code)
	}
	viewer, _ := login("audit@vendor.com", "viewer-password")
	var me api.Me
	call(t, ts, acme, "GET", "/api/v1/me", nil, &me)
	if me.Role != api.RoleOperator || !me.Scoped || me.Name != "it@acme.com" {
		t.Fatalf("me: %+v", me)
	}

	// --- viewer: read-only ---
	if code := call(t, ts, viewer, "GET", "/api/v1/devices", nil, nil); code != 200 {
		t.Fatalf("viewer read: %d", code)
	}
	if code := call(t, ts, viewer, "PATCH", "/api/v1/devices/"+globex.DeviceID, map[string]any{"name": "x"}, nil); code != 403 {
		t.Fatalf("viewer write: %d", code)
	}
	if code := call(t, ts, viewer, "GET", "/api/v1/users", nil, nil); code != 403 {
		t.Fatalf("viewer users: %d", code)
	}

	// --- client account: only acme devices ---
	var devs []api.Device
	call(t, ts, acme, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 2 {
		t.Fatalf("scoped list: %d devices", len(devs))
	}
	if code := call(t, ts, acme, "GET", "/api/v1/devices/"+globex.DeviceID, nil, nil); code != 404 {
		t.Fatalf("other customer's device visible: %d", code)
	}
	if code := call(t, ts, acme, "GET", "/api/v1/devices/"+globex.DeviceID+"/actions", nil, nil); code != 404 {
		t.Fatalf("other customer's actions visible: %d", code)
	}
	if code := call(t, ts, acme, "POST", "/api/v1/devices/"+globex.DeviceID+"/actions", api.CreateActionRequest{Type: "reboot"}, nil); code != 404 {
		t.Fatalf("action on other customer's device: %d", code)
	}
	if code := call(t, ts, acme, "POST", "/api/v1/devices/"+acme1.DeviceID+"/actions", api.CreateActionRequest{Type: "reboot"}, nil); code != 201 {
		t.Fatalf("action on own device: %d", code)
	}
	if code := call(t, ts, acme, "PATCH", "/api/v1/devices/"+acme2.DeviceID, map[string]any{"set_labels": map[string]string{"customer": "globex"}}, nil); code != 403 {
		t.Fatalf("moved device out of scope: %d", code)
	}
	if code := call(t, ts, acme, "PATCH", "/api/v1/devices/"+acme2.DeviceID, map[string]any{"name": "Front desk", "set_labels": map[string]string{"floor": "2"}}, nil); code != 200 {
		t.Fatalf("edit own device: %d", code)
	}
	if code := call(t, ts, acme, "PATCH", "/api/v1/devices/"+acme2.DeviceID, map[string]any{"license_revoked": true}, nil); code != 403 {
		t.Fatalf("client changed licensing: %d", code)
	}
	for _, path := range []string{"/api/v1/deployments", "/api/v1/releases", "/api/v1/history/fleet", "/api/v1/tokens", "/api/v1/monitors"} {
		if code := call(t, ts, acme, "GET", path, nil, nil); code != 403 {
			t.Errorf("client account reached fleet-wide %s: %d", path, code)
		}
	}
	var sum api.FleetSummary
	call(t, ts, acme, "GET", "/api/v1/summary", nil, &sum)
	if sum.Devices != 2 {
		t.Fatalf("scoped summary: %+v", sum)
	}
	var evs []api.Event
	call(t, ts, acme, "GET", "/api/v1/events?limit=100", nil, &evs)
	for _, e := range evs {
		if e.DeviceID == globex.DeviceID || e.DeviceID == "" {
			t.Fatalf("client sees foreign or fleet event: %+v", e)
		}
	}
	// Tokens a client creates always enroll into their scope.
	var tok api.EnrollmentToken
	call(t, ts, acme, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "globex", "site": "hq"}}, &tok)
	if tok.Labels["customer"] != "acme" || tok.Labels["site"] != "hq" {
		t.Fatalf("token labels: %v", tok.Labels)
	}

	// --- audit trail ---
	call(t, ts, admin, "GET", "/api/v1/events?limit=200", nil, &evs)
	found := false
	for _, e := range evs {
		if e.Type == "action.queued" && e.Actor == "it@acme.com" {
			found = true
		}
	}
	if !found {
		t.Fatal("action not attributed to the user")
	}

	// --- personal API tokens ---
	var pt api.APIToken
	call(t, ts, acme, "POST", "/api/v1/me/tokens", map[string]string{"name": "ci"}, &pt)
	if code := call(t, ts, pt.Token, "GET", "/api/v1/devices", nil, &devs); code != 200 || len(devs) != 2 {
		t.Fatalf("personal token: %d, %d devices", code, len(devs))
	}
	call(t, ts, acme, "DELETE", "/api/v1/me/tokens/"+pt.ID, nil, nil)
	if code := call(t, ts, pt.Token, "GET", "/api/v1/devices", nil, nil); code != 401 {
		t.Fatalf("revoked token still works: %d", code)
	}

	// --- password change signs out every session ---
	if code := call(t, ts, acme, "POST", "/api/v1/me/password", map[string]string{"current": "acme-password-1", "new": "acme-password-2"}, nil); code != 204 {
		t.Fatalf("change password: %d", code)
	}
	if code := call(t, ts, acme, "GET", "/api/v1/me", nil, nil); code != 401 {
		t.Fatalf("old session survived password change: %d", code)
	}
	acme, _ = login("it@acme.com", "acme-password-2")

	// --- disabling takes effect immediately; self-protection ---
	var users []api.User
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	byEmail := map[string]api.User{}
	for _, u := range users {
		byEmail[u.Email] = u
	}
	tr := true
	call(t, ts, admin, "PATCH", "/api/v1/users/"+byEmail["it@acme.com"].ID, map[string]any{"disabled": tr}, nil)
	if code := call(t, ts, acme, "GET", "/api/v1/devices", nil, nil); code != 401 {
		t.Fatalf("disabled user still signed in: %d", code)
	}
	ops, _ := login("ops@vendor.com", "correct horse battery")
	if code := call(t, ts, ops, "PATCH", "/api/v1/users/"+byEmail["ops@vendor.com"].ID, map[string]any{"role": "viewer"}, nil); code != 400 {
		t.Fatalf("demoted self: %d", code)
	}
	if code := call(t, ts, ops, "DELETE", "/api/v1/users/"+byEmail["ops@vendor.com"].ID, nil, nil); code != 400 {
		t.Fatalf("deleted self: %d", code)
	}
	if code := call(t, ts, ops, "GET", "/api/v1/backup", nil, nil); code != 200 {
		t.Fatalf("admin user backup: %d", code)
	}

	// --- brute force: five failures lock the email out ---
	for i := 0; i < 5; i++ {
		login("audit@vendor.com", "guess")
	}
	if _, code := login("audit@vendor.com", "viewer-password"); code != 429 {
		t.Fatalf("lockout: %d", code)
	}
}
