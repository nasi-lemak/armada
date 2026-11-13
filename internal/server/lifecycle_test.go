package server

import (
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// enrollWith enrolls a device with labels and returns its ID and key.
func enrollWith(t *testing.T, e *alertEnv, name string, labels map[string]string) (string, string) {
	t.Helper()
	var tok api.EnrollmentToken
	call(t, e.ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: labels}, &tok)
	var er api.EnrollResponse
	call(t, e.ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
	call(t, e.ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, nil)
	return er.DeviceID, er.DeviceKey
}

func setLifecycle(t *testing.T, e *alertEnv, id, state, note string) int {
	t.Helper()
	return call(t, e.ts, admin, "POST", "/api/v1/devices/"+id+"/lifecycle", api.LifecycleRequest{State: state, Note: note}, nil)
}

func getDevice(t *testing.T, e *alertEnv, id string) api.Device {
	t.Helper()
	var det struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+id, nil, &det)
	return det.Device
}

func TestLifecycleAlertsAndUpdates(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Device offline", Condition: api.ConditionDeviceOffline, OfflineMinutes: 10, NotifyResolved: true})
	e.heartbeat(t, nil)
	if d := getDevice(t, e, e.devID); d.Lifecycle != api.LifecycleActive {
		t.Fatalf("new device lifecycle %q", d.Lifecycle)
	}
	for _, bad := range []string{"", "broken", "ACTIVE "} {
		if code := setLifecycle(t, e, e.devID, bad+"x", ""); code != 400 {
			t.Errorf("state %q accepted: %d", bad, code)
		}
	}

	// It goes offline and alerts; sending it for repair closes the alert
	// without a misleading "resolved".
	e.clock = e.clock.Add(15 * time.Minute)
	e.tick(t)
	next(t, e.hook)
	if code := setLifecycle(t, e, e.devID, api.LifecycleRepair, "RMA 4411: cracked screen"); code != 200 {
		t.Fatalf("repair: %d", code)
	}
	e.tick(t)
	none(t, e.hook)
	if a := e.firing(t); len(a) != 0 {
		t.Fatalf("alert still open: %+v", a)
	}
	var all []api.Alert
	call(t, e.ts, admin, "GET", "/api/v1/alerts", nil, &all)
	if len(all) != 1 || !all[0].ClosedOutOfService {
		t.Fatalf("closed alert not marked out of service: %+v", all)
	}
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	none(t, e.hook) // and no new ones
	d := getDevice(t, e, e.devID)
	if d.Lifecycle != api.LifecycleRepair || d.LifecycleInfo == nil || d.LifecycleInfo.Note != "RMA 4411: cracked screen" || d.LifecycleInfo.ChangedBy == "" {
		t.Fatalf("repair state: %+v %+v", d.Lifecycle, d.LifecycleInfo)
	}

	// In repair: no updates. As a spare: updates (to stay current), no alerts.
	rel := upload(t, e.ts, "app", "2.0.0")
	var dep api.Deployment
	call(t, e.ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID, Strategy: &api.Strategy{Stages: []int{100}}}, &dep)
	check := func() *api.UpdateInstruction {
		var hr api.HeartbeatResponse
		call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}, Packages: map[string]string{"app": "1.0.0"}}, &hr)
		return hr.Update
	}
	if u := check(); u != nil {
		t.Fatalf("update offered in repair: %+v", u)
	}
	var det struct{ Targeted int }
	call(t, e.ts, admin, "GET", "/api/v1/deployments/"+dep.ID, nil, &det)
	if det.Targeted != 0 {
		t.Fatalf("device in repair counted as targeted: %d", det.Targeted)
	}
	setLifecycle(t, e, e.devID, api.LifecycleSpare, "back from repair")
	if u := check(); u == nil {
		t.Fatal("spare not offered the update")
	}
	e.clock = e.clock.Add(time.Hour)
	e.tick(t)
	none(t, e.hook)

	var sum api.FleetSummary
	call(t, e.ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.Devices != 1 || sum.Spare != 1 || sum.Offline != 0 {
		t.Fatalf("summary: %+v", sum)
	}
	// Back in service: alerts again.
	setLifecycle(t, e, e.devID, api.LifecycleActive, "")
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertFiring {
		t.Fatalf("active again: %+v", n)
	}
}

func TestRetireDevice(t *testing.T) {
	e := setupAlerts(t)
	other, _ := enrollWith(t, e, "kiosk-08", map[string]string{"site": "sg"})
	if code := setLifecycle(t, e, e.devID, api.LifecycleRetired, "end of life"); code != 200 {
		t.Fatalf("retire: %d", code)
	}
	// Its key is gone.
	if code := call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 401 {
		t.Fatalf("retired device checked in: %d", code)
	}
	d := getDevice(t, e, e.devID)
	if d.Lifecycle != api.LifecycleRetired || !d.LicenseRevoked {
		t.Fatalf("retired: %+v", d)
	}
	// Final: no way back, no actions.
	if code := setLifecycle(t, e, e.devID, api.LifecycleActive, ""); code != 409 {
		t.Fatalf("un-retired: %d", code)
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/devices/"+e.devID+"/actions", api.CreateActionRequest{Type: "reboot"}, nil); code != 409 {
		t.Fatalf("action on retired device: %d", code)
	}
	// Kept as a record, out of the fleet's lists and counts.
	list := func(q string) []string {
		var devs []api.Device
		call(t, e.ts, admin, "GET", "/api/v1/devices"+q, nil, &devs)
		var ids []string
		for _, d := range devs {
			ids = append(ids, d.ID)
		}
		return ids
	}
	if ids := list(""); len(ids) != 1 || ids[0] != other {
		t.Fatalf("default list: %v", ids)
	}
	if ids := list("?lifecycle=retired"); len(ids) != 1 || ids[0] != e.devID {
		t.Fatalf("retired list: %v", ids)
	}
	if ids := list("?lifecycle=all"); len(ids) != 2 {
		t.Fatalf("all: %v", ids)
	}
	if code := call(t, e.ts, admin, "GET", "/api/v1/devices?lifecycle=gone", nil, nil); code != 400 {
		t.Fatalf("bad filter: %d", code)
	}
	var sum api.FleetSummary
	call(t, e.ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.Devices != 1 || sum.Retired != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	var evs []api.Event
	call(t, e.ts, admin, "GET", "/api/v1/events?device_id="+e.devID, nil, &evs)
	if len(evs) == 0 || evs[0].Type != "device.lifecycle" || !strings.Contains(evs[0].Message, "in service → retired (end of life)") {
		t.Fatalf("event: %+v", evs)
	}
}

func TestReplaceDevice(t *testing.T) {
	e := setupAlerts(t)
	call(t, e.ts, admin, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"set_labels": map[string]string{"customer": "acme"}, "channel": "beta"}, nil)
	spare, spareKey := enrollWith(t, e, "stock-0042", nil)
	setLifecycle(t, e, spare, api.LifecycleSpare, "")
	retiredDev, _ := enrollWith(t, e, "old", nil)
	setLifecycle(t, e, retiredDev, api.LifecycleRetired, "")

	// A client operator can see the old device but can't do fleet work.
	call(t, e.ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, e.ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	client := lr.Token
	if code := call(t, e.ts, client, "POST", "/api/v1/devices/"+e.devID+"/replace", api.ReplaceRequest{ReplacementID: spare}, nil); code != 403 {
		t.Fatalf("client replaced a device: %d", code)
	}
	if code := call(t, e.ts, client, "POST", "/api/v1/devices/"+e.devID+"/lifecycle", api.LifecycleRequest{State: "retired"}, nil); code != 403 {
		t.Fatalf("client retired a device: %d", code)
	}

	for name, bad := range map[string]api.ReplaceRequest{
		"itself":          {ReplacementID: e.devID},
		"missing":         {ReplacementID: "dev_nope"},
		"bad old state":   {ReplacementID: spare, OldState: "active"},
		"retired as repl": {ReplacementID: retiredDev},
	} {
		if code := call(t, e.ts, admin, "POST", "/api/v1/devices/"+e.devID+"/replace", bad, nil); code < 400 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}

	var repl api.Device
	if code := call(t, e.ts, admin, "POST", "/api/v1/devices/"+e.devID+"/replace", api.ReplaceRequest{ReplacementID: spare, Note: "RMA 77"}, &repl); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	if repl.Name != "kiosk-07" || repl.Channel != "beta" || repl.Labels["customer"] != "acme" || repl.Labels["site"] != "sg" ||
		repl.Lifecycle != api.LifecycleActive || repl.LifecycleInfo.Replaces != e.devID {
		t.Fatalf("replacement: %+v %+v", repl, repl.LifecycleInfo)
	}
	old := getDevice(t, e, e.devID)
	if old.Name != "kiosk-07 (replaced)" || len(old.Labels) != 0 || old.Lifecycle != api.LifecycleRepair || old.LifecycleInfo.ReplacedBy != spare {
		t.Fatalf("old device: %+v %+v", old, old.LifecycleInfo)
	}
	// The client now sees the replacement and no longer the old unit.
	var devs []api.Device
	call(t, e.ts, client, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 1 || devs[0].ID != spare {
		t.Fatalf("client devices after replace: %+v", devs)
	}
	if code := call(t, e.ts, client, "GET", "/api/v1/devices/"+e.devID, nil, nil); code != 404 {
		t.Fatalf("client still sees the old unit: %d", code)
	}
	// The replacement keeps its own key; the old one (in repair) keeps its too.
	if code := call(t, e.ts, spareKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 200 {
		t.Fatalf("replacement key: %d", code)
	}
	if code := call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 200 {
		t.Fatalf("old key in repair: %d", code)
	}
	// Replacing again, retiring the old unit.
	other, _ := enrollWith(t, e, "stock-0043", nil)
	if code := call(t, e.ts, admin, "POST", "/api/v1/devices/"+spare+"/replace", api.ReplaceRequest{ReplacementID: other, OldState: "retired"}, nil); code != 200 {
		t.Fatalf("replace again: %d", code)
	}
	if code := call(t, e.ts, spareKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 401 {
		t.Fatalf("retired replacement still checks in: %d", code)
	}
	if code := call(t, e.ts, admin, "POST", "/api/v1/devices/"+spare+"/replace", api.ReplaceRequest{ReplacementID: e.devID}, nil); code != 409 {
		t.Fatalf("replaced a retired device: %d", code)
	}
	var evs []api.Event
	call(t, e.ts, admin, "GET", "/api/v1/events?device_id="+e.devID, nil, &evs)
	if len(evs) == 0 || evs[0].Type != "device.replaced" || !strings.Contains(evs[0].Message, "RMA 77") {
		t.Fatalf("replace event: %+v", evs)
	}
}
