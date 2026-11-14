package server

import (
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestDeviceInventory(t *testing.T) {
	e := setupAlerts(t) // kiosk-07, site=sg
	// The serial comes from the device.
	call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Serial: "C02XK0ABJG5H"}}, nil)
	if d := getDevice(t, e, e.devID); d.Info.Serial != "C02XK0ABJG5H" || d.Inventory != nil {
		t.Fatalf("device: %+v %+v", d.Info, d.Inventory)
	}

	// Operators record the rest; viewers can't.
	for _, u := range []struct{ email, role string }{{"it@acme.com", api.RoleOperator}, {"ceo@acme.com", api.RoleViewer}} {
		call(t, e.ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: u.email, Role: u.role,
			Scope: api.Selector{Labels: map[string]string{"site": "sg"}}, Password: "long-password-1"}, nil)
	}
	login := func(email string) string {
		var lr api.LoginResponse
		call(t, e.ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	op, viewer := login("it@acme.com"), login("ceo@acme.com")
	inv := api.Inventory{AssetTag: " ACME-00417 ", Location: "Orchard store, till 3", Contact: "Mei, +65 5550 1234", Notes: "Key in the drawer"}
	if code := call(t, e.ts, viewer, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"inventory": inv}, nil); code != 403 {
		t.Fatalf("viewer edited inventory: %d", code)
	}
	if code := call(t, e.ts, op, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"inventory": api.Inventory{AssetTag: strings.Repeat("x", 65)}}, nil); code != 400 {
		t.Fatalf("long asset tag: %d", code)
	}
	var d api.Device
	if code := call(t, e.ts, op, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"inventory": inv}, &d); code != 200 || d.Inventory == nil || d.Inventory.AssetTag != "ACME-00417" {
		t.Fatalf("edit: %d %+v", code, d.Inventory)
	}
	// Other edits leave it alone; it shows in lists and reports.
	call(t, e.ts, op, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"name": "Till 3"}, nil)
	var devs []api.Device
	call(t, e.ts, viewer, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 1 || devs[0].Inventory == nil || devs[0].Inventory.Location != "Orchard store, till 3" || devs[0].Name != "Till 3" {
		t.Fatalf("list: %+v", devs)
	}
	var rep api.Report
	call(t, e.ts, admin, "GET", "/api/v1/reports?period=this-month", nil, &rep)
	if len(rep.Devices) != 1 || rep.Devices[0].Serial != "C02XK0ABJG5H" || rep.Devices[0].AssetTag != "ACME-00417" {
		t.Fatalf("report: %+v", rep.Devices)
	}
	// Clearing it.
	call(t, e.ts, op, "PATCH", "/api/v1/devices/"+e.devID, map[string]any{"inventory": api.Inventory{}}, nil)
	if d := getDevice(t, e, e.devID); d.Inventory != nil {
		t.Fatalf("not cleared: %+v", d.Inventory)
	}
}
