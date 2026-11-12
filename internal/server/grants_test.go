package server

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestPerClientGrants(t *testing.T) {
	_, ts := setup(t)
	devs := map[string]string{}
	for _, c := range []string{"acme", "globex", "initech"} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: c, Info: api.DeviceInfo{OS: "linux"}}, &er)
		devs[c] = er.DeviceID
	}
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	globex := api.Selector{Labels: map[string]string{"customer": "globex"}}

	// Support engineer: sees the whole fleet, operates only Acme's devices.
	var u api.User
	if c := call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "sam@vendor.com", Password: "support-password",
		Grants: []api.Grant{{Role: api.RoleViewer}, {Role: api.RoleOperator, Scope: acme}}}, &u); c != 201 || u.Role != api.RoleOperator {
		t.Fatalf("create with grants: %d %+v", c, u)
	}
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "sam@vendor.com", Password: "support-password"}, &lr)
	var me api.Me
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.Scoped || me.FleetRole != api.RoleViewer || me.Role != api.RoleOperator {
		t.Fatalf("me: %+v", me)
	}
	var ds []api.Device
	call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, &ds)
	if len(ds) != 3 {
		t.Fatalf("fleet viewer sees %d devices", len(ds))
	}
	act := func(dev string) int {
		return call(t, ts, lr.Token, "POST", "/api/v1/devices/"+dev+"/actions", api.CreateActionRequest{Type: "reboot"}, nil)
	}
	if act(devs["acme"]) != 201 || act(devs["globex"]) != 403 || act(devs["initech"]) != 403 {
		t.Fatal("per-device operator rights wrong")
	}
	// Fleet pages need a fleet-wide grant of the right level.
	if c := call(t, ts, lr.Token, "GET", "/api/v1/deployments", nil, nil); c != 200 {
		t.Fatalf("fleet viewer reading deployments: %d", c)
	}
	if c := call(t, ts, lr.Token, "POST", "/api/v1/monitors", map[string]any{"name": "x", "check": map[string]any{"type": "tcp", "target": "a:1"}}, nil); c != 403 {
		t.Fatalf("client operator wrote a fleet-wide monitor: %d", c)
	}
	// Can't relabel a device out of where they're an operator.
	if c := call(t, ts, lr.Token, "PATCH", "/api/v1/devices/"+devs["acme"], map[string]any{"set_labels": map[string]string{"customer": "globex"}}, nil); c != 403 {
		t.Fatalf("moved device to where they're only a viewer: %d", c)
	}
	// Licensing needs a fleet-wide operator.
	if c := call(t, ts, lr.Token, "PATCH", "/api/v1/devices/"+devs["acme"], map[string]any{"license_revoked": true}, nil); c != 403 {
		t.Fatalf("client operator changed licensing: %d", c)
	}
	// Enrollment tokens go to Acme (their only operator grant).
	var et api.EnrollmentToken
	call(t, ts, lr.Token, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "initech"}}, &et)
	if et.Labels["customer"] != "acme" {
		t.Fatalf("token labels: %v", et.Labels)
	}

	// Edit: operator for Acme, viewer for Globex, nothing else.
	if c := call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"grants": []api.Grant{{Role: api.RoleOperator, Scope: acme}, {Role: api.RoleViewer, Scope: globex}}}, &u); c != 200 {
		t.Fatalf("edit grants: %d", c)
	}
	call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, &ds)
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if len(ds) != 2 || !me.Scoped {
		t.Fatalf("client grants: %d devices, %+v", len(ds), me)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices/"+devs["initech"], nil, nil); c != 404 {
		t.Fatalf("device outside every grant: %d", c)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/deployments", nil, nil); c != 403 {
		t.Fatalf("client-only account on a fleet page: %d", c)
	}
	// A single role change is refused while grants exist; clearing them
	// goes back to one role and scope.
	if c := call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"role": "viewer"}, nil); c != 400 {
		t.Fatalf("role edit over grants: %d", c)
	}
	var single api.User
	call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"grants": []api.Grant{}, "role": "viewer", "scope": globex}, &single)
	if len(single.Grants) != 0 || single.Role != api.RoleViewer || single.Scope.Labels["customer"] != "globex" {
		t.Fatalf("back to single: %+v", single)
	}
	// Validation: admin can't be scoped; grants can't be empty-role.
	for _, bad := range [][]api.Grant{{{Role: api.RoleAdmin, Scope: acme}}, {{Role: "root"}}} {
		if c := call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"grants": bad}, nil); c != 400 {
			t.Errorf("accepted %v: %d", bad, c)
		}
	}
}
