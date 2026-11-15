package server

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestNetCheckAction(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "till", Info: api.DeviceInfo{OS: "linux"}}, &er)
	// A client operator may run it: it changes nothing on the device.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	var a api.Action
	if code := call(t, ts, lr.Token, "POST", "/api/v1/devices/"+er.DeviceID+"/actions", api.CreateActionRequest{Type: api.ActionNetCheck, Args: []string{"ignored"}}, &a); code != 201 || a.Type != "netcheck" || len(a.Args) != 0 {
		t.Fatalf("netcheck: %d %+v", code, a)
	}
	// The agent gets it on its next check-in.
	var hb api.HeartbeatResponse
	call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, &hb)
	if len(hb.Actions) != 1 || hb.Actions[0].Type != "netcheck" {
		t.Fatalf("delivered: %+v", hb.Actions)
	}
	// And as a bulk operation.
	var b api.BulkOperation
	if code := call(t, ts, admin, "POST", "/api/v1/bulk", api.BulkRequest{Operation: api.ActionNetCheck, All: true, DryRun: true}, &b); code != 200 || len(b.Applied) != 1 {
		t.Fatalf("bulk: %d %+v", code, b)
	}
}
