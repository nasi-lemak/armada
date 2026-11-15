package server

import (
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// Regression tests for the security review.

func TestReviewFixes(t *testing.T) {
	s, ts := setup(t)
	user := func(email, customer, role string) string {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: role,
			Scope: api.Selector{Labels: map[string]string{"customer": customer}}, Password: "long-password-1"}, nil)
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	acme, globex := user("op@acme.com", "acme", api.RoleOperator), user("op@globex.com", "globex", api.RoleOperator)
	enroll := func(token, name, serial string) string {
		var tok api.EnrollmentToken
		if code := call(t, ts, token, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok); code != 201 {
			t.Fatalf("token: %d", code)
		}
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux", Serial: serial}}, &er)
		return er.DeviceID
	}

	// A client can't claim a vendor pre-registration by reporting its serial.
	if code, res := importCSV(t, ts, admin, "serial,name,location\nSN-VIP,acme-ceo-kiosk,Board room\n", ""); code != 200 || !res.Applied {
		t.Fatalf("import: %d %+v", code, res)
	}
	fake := enroll(globex, "fake", "SN-VIP")
	var det struct {
		Device *api.Device `json:"device"`
	}
	call(t, ts, admin, "GET", "/api/v1/devices/"+fake, nil, &det)
	if det.Device.Name != "fake" || det.Device.Inventory != nil {
		t.Fatalf("client's device took the vendor registration: %+v", det.Device)
	}
	var regs []api.Registration
	call(t, ts, admin, "GET", "/api/v1/registrations", nil, &regs)
	if len(regs) != 1 || regs[0].ClaimedBy != "" {
		t.Fatalf("registration consumed: %+v", regs)
	}
	// The vendor's own enrollment still gets it.
	real := enroll(admin, "pi", "SN-VIP")
	det.Device = nil
	call(t, ts, admin, "GET", "/api/v1/devices/"+real, nil, &det)
	if det.Device.Name != "acme-ceo-kiosk" {
		t.Fatalf("vendor enrollment: %+v", det.Device)
	}

	// A vendor maintenance window on a client's device: the client's
	// timeline doesn't name who made it.
	dev := enroll(acme, "till", "SN-1")
	end := time.Now().Add(time.Hour)
	if code := call(t, ts, admin, "POST", "/api/v1/maintenance", api.Maintenance{Name: "Vendor work", Selector: api.Selector{DeviceIDs: []string{dev}},
		StartsAt: time.Now(), EndsAt: &end}, nil); code != 201 {
		t.Fatalf("maintenance: %d", code)
	}
	var tl api.Timeline
	call(t, ts, acme, "GET", "/api/v1/devices/"+dev+"/timeline?kinds=maintenance", nil, &tl)
	if len(tl.Entries) != 1 || tl.Entries[0].Actor != "" || tl.Entries[0].Link != "" {
		t.Fatalf("client timeline: %+v", tl.Entries)
	}
	tl = api.Timeline{}
	call(t, ts, admin, "GET", "/api/v1/devices/"+dev+"/timeline?kinds=maintenance", nil, &tl)
	if len(tl.Entries) != 1 || tl.Entries[0].Actor == "" {
		t.Fatalf("vendor timeline: %+v", tl.Entries)
	}
	// An absurd cursor is refused rather than lifting the limit.
	if code := call(t, ts, admin, "GET", "/api/v1/devices/"+dev+"/timeline?cursor=0.9223372036854775807", nil, nil); code != 400 {
		t.Fatalf("cursor: %d", code)
	}

	// A client webhook pointed at the server's own network: the error
	// doesn't say where the name resolved.
	s.allowPrivateClientTargets = false
	var hk api.EventHook
	if code := call(t, ts, acme, "POST", "/api/v1/event-hooks", api.EventHook{Name: "x", URL: "https://localhost:1/x", Enabled: true}, &hk); code != 201 {
		t.Fatalf("hook: %d", code)
	}
	var res map[string]any
	call(t, ts, acme, "POST", "/api/v1/event-hooks/"+hk.ID+"/test", nil, &res)
	if msg, _ := res["error"].(string); res["ok"] != false || strings.Contains(msg, "127.0.0.1") || strings.Contains(msg, "::1") || msg != errNonPublic.Error() {
		t.Fatalf("test result: %v", res)
	}
}

func TestVendorSiteOutageHasNoDevice(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Site down", Condition: api.ConditionSiteOutage, MinDevices: 1, MinPercent: 1})
	e.heartbeat(t, nil)
	e.clock = e.clock.Add(10 * time.Minute)
	e.tick(t)
	var alerts []api.Alert
	call(t, e.ts, admin, "GET", "/api/v1/alerts?status=firing", nil, &alerts)
	if len(alerts) != 1 || alerts[0].DeviceID != "" {
		t.Fatalf("vendor site alert: %+v", alerts)
	}
}
