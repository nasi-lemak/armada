package server

import (
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func (e *alertEnv) maintenance(t *testing.T, token string, m api.Maintenance) (api.Maintenance, int) {
	t.Helper()
	var out api.Maintenance
	code := call(t, e.ts, token, "POST", "/api/v1/maintenance", m, &out)
	return out, code
}

func (e *alertEnv) firing(t *testing.T) []api.Alert {
	t.Helper()
	var alerts []api.Alert
	call(t, e.ts, admin, "GET", "/api/v1/alerts?status=firing", nil, &alerts)
	return alerts
}

func TestMaintenanceHoldsAlerts(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Device offline", Condition: api.ConditionDeviceOffline, OfflineMinutes: 10, NotifyResolved: true, RepeatMinutes: 30})
	e.rule(t, api.AlertRule{Name: "Update failed", Condition: api.ConditionUpdateFailed})
	e.heartbeat(t, nil)

	end := e.clock.Add(2 * time.Hour)
	m, code := e.maintenance(t, admin, api.Maintenance{Note: "replacing the screen", Selector: api.Selector{DeviceIDs: []string{e.devID}}, EndsAt: &end})
	if code != 201 || !m.Active || m.Name != "Maintenance on kiosk-07" || m.CreatedBy == "" {
		t.Fatalf("create: %d %+v", code, m)
	}
	var d struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID, nil, &d)
	if d.Device.Maintenance == nil || d.Device.Maintenance.ID != m.ID || !d.Device.Maintenance.Alerts || d.Device.Maintenance.Until == nil {
		t.Fatalf("device not marked: %+v", d.Device.Maintenance)
	}

	// The device goes dark during the work: no alert, nor for failures.
	e.clock = e.clock.Add(30 * time.Minute)
	e.tick(t)
	e.s.event(t.Context(), "update.failed", e.devID, "dep_x", "install failed")
	none(t, e.hook)
	if a := e.firing(t); len(a) != 0 {
		t.Fatalf("alert during maintenance: %+v", a)
	}

	// Still down when it ends: now it alerts.
	e.clock = end.Add(time.Minute)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertFiring || n.Rule != "Device offline" {
		t.Fatalf("after maintenance: %+v", n)
	}

	// A new window over a firing alert: it neither repeats nor resolves.
	end2 := e.clock.Add(3 * time.Hour)
	m2, _ := e.maintenance(t, admin, api.Maintenance{Selector: api.Selector{Labels: map[string]string{"site": "sg"}}, EndsAt: &end2})
	if a := e.firing(t); len(a) != 1 || !a[0].InMaintenance {
		t.Fatalf("alert not marked in maintenance: %+v", a)
	}
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	none(t, e.hook) // no repeat
	e.heartbeat(t, nil)
	e.tick(t)
	none(t, e.hook) // no resolve either
	if a := e.firing(t); len(a) != 1 {
		t.Fatalf("alert resolved during maintenance: %+v", a)
	}
	// Ending it early lets the alert resolve.
	if code := call(t, e.ts, admin, "POST", "/api/v1/maintenance/"+m2.ID+"/end", nil, nil); code != 200 {
		t.Fatalf("end: %d", code)
	}
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved {
		t.Fatalf("resolve after maintenance: %+v", n)
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/maintenance/"+m2.ID+"/end", nil, nil); code != 409 {
		t.Fatalf("end twice: %d", code)
	}

	// Alerts-only maintenance doesn't touch updates, and vice versa.
	f := false
	end3 := e.clock.Add(time.Hour)
	m3, _ := e.maintenance(t, admin, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{e.devID}}, EndsAt: &end3, SuppressAlerts: &f})
	e.clock = e.clock.Add(20 * time.Minute)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertFiring {
		t.Fatalf("updates-only maintenance held an alert: %+v", n)
	}
	call(t, e.ts, admin, "DELETE", "/api/v1/maintenance/"+m3.ID, nil, nil)

	var list []api.Maintenance
	call(t, e.ts, admin, "GET", "/api/v1/maintenance", nil, &list)
	if len(list) != 2 || list[0].State != "ended" {
		t.Fatalf("list: %+v", list)
	}
	var evs []api.Event
	call(t, e.ts, admin, "GET", "/api/v1/events?limit=200", nil, &evs)
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Type] = true
	}
	for _, typ := range []string{"maintenance.created", "maintenance.ended", "maintenance.deleted"} {
		if !seen[typ] {
			t.Errorf("no %s event", typ)
		}
	}
}

func TestMaintenanceValidationAndRecurring(t *testing.T) {
	e := setupAlerts(t)
	// Sunday 2026-11-15 03:00 UTC.
	e.clock = time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	past, far := e.clock.Add(-time.Hour), e.clock.Add(40*24*time.Hour)
	soon := e.clock.Add(time.Hour)
	f := false
	for name, bad := range map[string]api.Maintenance{
		"no devices":     {EndsAt: &soon},
		"no end":         {Selector: api.Selector{Channel: "stable"}},
		"ended":          {Selector: api.Selector{Channel: "stable"}, StartsAt: past.Add(-time.Hour), EndsAt: &past},
		"too long":       {Selector: api.Selector{Channel: "stable"}, EndsAt: &far},
		"bad window":     {Selector: api.Selector{Channel: "stable"}, Windows: "2am-4am"},
		"bad day":        {Selector: api.Selector{Channel: "stable"}, Windows: "02:00-04:00", Days: []string{"funday"}},
		"bad zone":       {Selector: api.Selector{Channel: "stable"}, Windows: "02:00-04:00", TimeZone: "Mars/Olympus"},
		"does nothing":   {Selector: api.Selector{Channel: "stable"}, EndsAt: &soon, SuppressAlerts: &f, PauseUpdates: &f},
		"unknown device": {Selector: api.Selector{DeviceIDs: []string{"dev_nope"}}, EndsAt: &soon},
	} {
		if _, code := e.maintenance(t, admin, bad); code != 400 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}

	// Weekly: Sundays 02:00-04:00 in Singapore (UTC+8) = Saturday 18:00-20:00 UTC.
	m, code := e.maintenance(t, admin, api.Maintenance{Name: "Weekly reboot", Selector: api.Selector{Labels: map[string]string{"site": "sg"}},
		Windows: "02:00-04:00", Days: []string{"Sunday"}, TimeZone: "Asia/Singapore"})
	if code != 201 || m.Active || m.State != "waiting" || m.Days[0] != "sun" {
		t.Fatalf("recurring: %d %+v", code, m)
	}
	e.clock = time.Date(2026, 11, 21, 18, 30, 0, 0, time.UTC) // Sunday 02:30 in Singapore
	var d struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID, nil, &d)
	if mi := d.Device.Maintenance; mi == nil || mi.Until == nil || !mi.Until.Equal(time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("in window: %+v", mi)
	}
	e.clock = e.clock.Add(2 * time.Hour)
	var after struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID, nil, &after)
	if after.Device.Maintenance != nil {
		t.Fatalf("outside window: %+v", after.Device.Maintenance)
	}

	// A scheduled one-off is listed as scheduled and can be cancelled.
	start, stop := e.clock.Add(24*time.Hour), e.clock.Add(26*time.Hour)
	sch, _ := e.maintenance(t, admin, api.Maintenance{Selector: api.Selector{Channel: "stable"}, StartsAt: start, EndsAt: &stop})
	if sch.State != "scheduled" {
		t.Fatalf("scheduled: %+v", sch)
	}
	var ended api.Maintenance
	call(t, e.ts, admin, "POST", "/api/v1/maintenance/"+sch.ID+"/end", nil, &ended)
	if ended.State != "ended" {
		t.Fatalf("cancelled: %+v", ended)
	}
	// Long-ended windows are pruned (after reports stop needing them).
	e.clock = e.clock.Add(101 * 24 * time.Hour)
	e.tick(t)
	var list []api.Maintenance
	call(t, e.ts, admin, "GET", "/api/v1/maintenance?all=1", nil, &list)
	if len(list) != 1 || list[0].ID != m.ID {
		t.Fatalf("after prune: %+v", list)
	}
}

func TestMaintenancePausesUpdates(t *testing.T) {
	e := setupAlerts(t)
	rel := upload(t, e.ts, "app", "2.0.0")
	var dep api.Deployment
	if code := call(t, e.ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID,
		Strategy: &api.Strategy{Stages: []int{100}}}, &dep); code != 201 {
		t.Fatalf("deployment: %d", code)
	}
	check := func() *api.UpdateInstruction {
		var hr api.HeartbeatResponse
		call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
			Packages: map[string]string{"app": "1.0.0"}}, &hr)
		return hr.Update
	}
	end := e.clock.Add(time.Hour)
	m, _ := e.maintenance(t, admin, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{e.devID}}, EndsAt: &end})
	if u := check(); u != nil {
		t.Fatalf("update offered during maintenance: %+v", u)
	}
	call(t, e.ts, admin, "POST", "/api/v1/maintenance/"+m.ID+"/end", nil, nil)
	if u := check(); u == nil || u.Version != "2.0.0" {
		t.Fatalf("update not offered after maintenance: %+v", u)
	}
	// An install already under way is not interrupted by a new window.
	end2 := e.clock.Add(time.Hour)
	e.maintenance(t, admin, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{e.devID}}, EndsAt: &end2})
	if u := check(); u == nil {
		t.Fatal("in-progress update withdrawn by maintenance")
	}
}

func TestMaintenanceClientScoping(t *testing.T) {
	_, ts := setup(t)
	devs := map[string]string{}
	for _, c := range []string{"acme", "globex"} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: c + "-kiosk", Info: api.DeviceInfo{OS: "linux"}}, &er)
		devs[c] = er.DeviceID
	}
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	users := map[string]string{}
	for email, g := range map[string]api.Grant{
		"it@acme.com": {Role: api.RoleOperator, Scope: acme}, "ceo@acme.com": {Role: api.RoleViewer, Scope: acme},
		"auditor@vendor.com": {Role: api.RoleViewer},
	} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: g.Role, Scope: g.Scope, Password: "long-password-1"}, nil)
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		users[email] = lr.Token
	}
	acmeIT, acmeCEO, auditor := users["it@acme.com"], users["ceo@acme.com"], users["auditor@vendor.com"]
	end := time.Now().Add(time.Hour)
	mk := func(token string, m api.Maintenance) (api.Maintenance, int) {
		m.EndsAt = &end
		var out api.Maintenance
		return out, call(t, ts, token, "POST", "/api/v1/maintenance", m, &out)
	}

	// A client maintenance on "all devices" only covers the client's own.
	all, code := mk(acmeIT, api.Maintenance{Selector: api.Selector{Labels: map[string]string{"customer": "*"}}})
	if code != 201 || !sameScope(all.ClientScope, acme) {
		t.Fatalf("client create: %d %+v", code, all)
	}
	var dv []api.Device
	call(t, ts, admin, "GET", "/api/v1/devices", nil, &dv)
	for _, d := range dv {
		if (d.Maintenance != nil) != (d.ID == devs["acme"]) {
			t.Fatalf("client maintenance reached %s: %+v", d.Name, d.Maintenance)
		}
	}
	// By device ID: own devices only; never another client's.
	if _, code := mk(acmeIT, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{devs["acme"]}}}); code != 201 {
		t.Fatalf("own device: %d", code)
	}
	if _, code := mk(acmeIT, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{devs["globex"]}}}); code != 400 {
		t.Fatalf("other client's device: %d", code)
	}
	if _, code := mk(acmeIT, api.Maintenance{Selector: acme, ClientScope: api.Selector{Labels: map[string]string{"customer": "globex"}}}); code != 403 {
		t.Fatalf("other client's scope: %d", code)
	}
	// Viewers can't plan maintenance.
	for _, tok := range []string{acmeCEO, auditor} {
		if _, code := mk(tok, api.Maintenance{Selector: api.Selector{DeviceIDs: []string{devs["acme"]}}}); code != 403 {
			t.Fatalf("viewer created maintenance: %d", code)
		}
	}
	// Vendor maintenance is invisible to the client (their device still shows it).
	vend, _ := mk(admin, api.Maintenance{Name: "Datacenter move", Selector: api.Selector{Labels: map[string]string{"customer": "*"}}})
	var list []api.Maintenance
	call(t, ts, acmeCEO, "GET", "/api/v1/maintenance", nil, &list)
	if len(list) != 2 {
		t.Fatalf("client list: %+v", list)
	}
	for _, m := range list {
		if m.ID == vend.ID {
			t.Fatal("client sees vendor maintenance")
		}
	}
	if code := call(t, ts, acmeIT, "POST", "/api/v1/maintenance/"+vend.ID+"/end", nil, nil); code != 404 {
		t.Fatalf("client ended vendor maintenance: %d", code)
	}
	if code := call(t, ts, acmeCEO, "POST", "/api/v1/maintenance/"+all.ID+"/end", nil, nil); code != 403 {
		t.Fatalf("client viewer ended maintenance: %d", code)
	}
	if code := call(t, ts, auditor, "DELETE", "/api/v1/maintenance/"+all.ID, nil, nil); code != 403 {
		t.Fatalf("fleet viewer deleted maintenance: %d", code)
	}
	if code := call(t, ts, acmeIT, "DELETE", "/api/v1/maintenance/"+all.ID, nil, nil); code != 204 {
		t.Fatalf("client deleted own maintenance: %d", code)
	}
	call(t, ts, auditor, "GET", "/api/v1/maintenance", nil, &list)
	if len(list) != 2 {
		t.Fatalf("fleet viewer list: %+v", list)
	}
}

func TestAlertAcknowledgement(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Disk", Condition: api.ConditionHealth, MinSeverity: api.HealthWarning, RepeatMinutes: 30})
	warn := api.CheckResult{Name: "disk /", Kind: "storage", Status: api.HealthWarning, Message: "85% full"}
	e.heartbeat(t, []api.CheckResult{warn})
	e.tick(t)
	next(t, e.hook)
	id := e.firing(t)[0].ID

	// Permissions: viewers can't; operators on the device can.
	call(t, e.ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "noc@vendor.com", Role: api.RoleViewer, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, e.ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "noc@vendor.com", Password: "long-password-1"}, &lr)
	if code := call(t, e.ts, lr.Token, "POST", "/api/v1/alerts/"+id+"/ack", api.AckRequest{}, nil); code != 403 {
		t.Fatalf("viewer acked: %d", code)
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/alerts/alr_nope/ack", api.AckRequest{}, nil); code != 404 {
		t.Fatalf("unknown alert: %d", code)
	}
	var a api.Alert
	if code := call(t, e.ts, admin, "POST", "/api/v1/alerts/"+id+"/ack", api.AckRequest{Note: "cleaning logs"}, &a); code != 200 || a.AckedAt == nil || a.AckNote != "cleaning logs" {
		t.Fatalf("ack: %d %+v", code, a)
	}

	// Acknowledged: no repeats.
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	none(t, e.hook)

	// Getting worse notifies and clears the acknowledgement.
	crit := warn
	crit.Status, crit.Message = api.HealthCritical, "99% full"
	e.heartbeat(t, []api.CheckResult{crit})
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Severity != api.HealthCritical {
		t.Fatalf("escalation: %+v", n)
	}
	if f := e.firing(t); f[0].AckedAt != nil {
		t.Fatalf("ack survived escalation: %+v", f[0])
	}
	// Repeats are back until acknowledged again; unack restores them.
	call(t, e.ts, admin, "POST", "/api/v1/alerts/"+id+"/ack", nil, nil)
	var un api.Alert
	call(t, e.ts, admin, "POST", "/api/v1/alerts/"+id+"/unack", nil, &un)
	if un.AckedAt != nil || un.ID != id {
		t.Fatalf("unack: %+v", un)
	}
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	next(t, e.hook)

	// Resolved alerts can't be acknowledged.
	ok := warn
	ok.Status, ok.Message = api.HealthOK, "40% full"
	e.heartbeat(t, []api.CheckResult{ok})
	e.tick(t)
	if code := call(t, e.ts, admin, "POST", "/api/v1/alerts/"+id+"/ack", nil, nil); code != 409 {
		t.Fatalf("ack resolved: %d", code)
	}
	var evs []api.Event
	call(t, e.ts, admin, "GET", "/api/v1/events?limit=200", nil, &evs)
	found := false
	for _, ev := range evs {
		found = found || (ev.Type == "alert.acknowledged" && strings.Contains(ev.Message, "cleaning logs"))
	}
	if !found {
		t.Fatal("no acknowledgement event")
	}
}
