package server

import (
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func intp(n int) *int { return &n }

func TestBulkOperations(t *testing.T) {
	e := setupAlerts(t) // kiosk-07, site=sg
	a1, k1 := enrollWith(t, e, "acme-1", map[string]string{"customer": "acme"})
	a2, _ := enrollWith(t, e, "acme-2", map[string]string{"customer": "acme"})
	g1, _ := enrollWith(t, e, "globex-1", map[string]string{"customer": "globex"})
	acme := api.Selector{Labels: map[string]string{"customer": "acme"}}
	bulk := func(token string, req api.BulkRequest) (api.BulkOperation, int) {
		t.Helper()
		var b api.BulkOperation
		code := call(t, e.ts, token, "POST", "/api/v1/bulk", req, &b)
		return b, code
	}

	// Safeguards: say which devices, preview first, and match the preview.
	if _, code := bulk(admin, api.BulkRequest{Operation: "reboot", DryRun: true}); code != 400 {
		t.Fatalf("empty selector: %d", code)
	}
	if _, code := bulk(admin, api.BulkRequest{Operation: "reboot", Selector: acme}); code != 400 {
		t.Fatalf("no expect: %d", code)
	}
	if _, code := bulk(admin, api.BulkRequest{Operation: "reboot", Selector: acme, Expect: intp(3)}); code != 409 {
		t.Fatalf("wrong expect: %d", code)
	}
	for name, req := range map[string]api.BulkRequest{
		"wipe": {Operation: "wipe"}, "logs": {Operation: "logs"}, "unknown": {Operation: "explode"},
		"exec without command": {Operation: "exec"}, "bad channel": {Operation: "set_channel", Channel: "no spaces"},
		"bad lifecycle": {Operation: "lifecycle", State: "gone"}, "no labels": {Operation: "set_labels"},
	} {
		req.Selector, req.DryRun = acme, true
		if _, code := bulk(admin, req); code != 400 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}

	// Preview, then run.
	prev, code := bulk(admin, api.BulkRequest{Operation: "reboot", Selector: acme, DryRun: true})
	if code != 200 || prev.ID != "" || prev.Matched != 2 || len(prev.Applied) != 2 {
		t.Fatalf("dry run: %d %+v", code, prev)
	}
	var acts []api.Action
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+a1+"/actions", nil, &acts)
	if len(acts) != 0 {
		t.Fatalf("dry run queued actions: %+v", acts)
	}
	b, code := bulk(admin, api.BulkRequest{Operation: "reboot", Selector: acme, Expect: intp(prev.Matched)})
	if code != 201 || b.ID == "" || len(b.Applied) != 2 || b.Applied[0].ActionID == "" || b.Progress[api.ActionQueued] != 2 {
		t.Fatalf("run: %d %+v", code, b)
	}
	// The device runs it; progress follows.
	var hr api.HeartbeatResponse
	call(t, e.ts, k1, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, &hr)
	if len(hr.Actions) != 1 || hr.Actions[0].BatchID != b.ID {
		t.Fatalf("device actions: %+v", hr.Actions)
	}
	call(t, e.ts, k1, "POST", "/api/v1/agent/actions/"+hr.Actions[0].ID+"/result", api.ActionResult{Status: api.ActionSucceeded}, nil)
	var got api.BulkOperation
	call(t, e.ts, admin, "GET", "/api/v1/bulk/"+b.ID, nil, &got)
	if got.Progress[api.ActionSucceeded] != 1 || got.Progress[api.ActionQueued] != 1 {
		t.Fatalf("progress: %+v", got.Progress)
	}

	// Each device is checked on its own: lock is Android-only.
	lock, _ := bulk(admin, api.BulkRequest{Operation: "lock", All: true, DryRun: true})
	if lock.Matched != 4 || len(lock.Applied) != 0 || len(lock.Skipped) != 4 || !strings.Contains(lock.Skipped[0].Reason, "Android") {
		t.Fatalf("lock: %+v", lock)
	}

	// Labels, channel and lifecycle.
	run := func(req api.BulkRequest) api.BulkOperation {
		t.Helper()
		req.DryRun = true
		p, _ := bulk(admin, req)
		req.DryRun, req.Expect = false, intp(p.Matched)
		out, code := bulk(admin, req)
		if code != 201 {
			t.Fatalf("%s: %d", req.Operation, code)
		}
		return out
	}
	run(api.BulkRequest{Operation: "set_labels", Selector: acme, Labels: map[string]string{"floor": "2"}})
	run(api.BulkRequest{Operation: "set_channel", Selector: api.Selector{DeviceIDs: []string{a1, g1}}, Channel: "beta"})
	run(api.BulkRequest{Operation: "remove_labels", Selector: api.Selector{DeviceIDs: []string{a2}}, LabelKeys: []string{"floor"}})
	if d := getDevice(t, e, a1); d.Labels["floor"] != "2" || d.Channel != "beta" {
		t.Fatalf("a1: %+v", d)
	}
	if d := getDevice(t, e, a2); d.Labels["floor"] != "" || d.Channel != "stable" || d.Labels["customer"] != "acme" {
		t.Fatalf("a2: %+v", d)
	}
	spare := run(api.BulkRequest{Operation: "lifecycle", Selector: acme, State: "spare", Note: "contract ended"})
	if len(spare.Applied) != 2 || getDevice(t, e, a2).Lifecycle != api.LifecycleSpare {
		t.Fatalf("lifecycle: %+v", spare)
	}
	again, _ := bulk(admin, api.BulkRequest{Operation: "lifecycle", Selector: acme, State: "spare", DryRun: true})
	if len(again.Skipped) != 2 || again.Skipped[0].Reason != "already spare" {
		t.Fatalf("lifecycle again: %+v", again)
	}

	// A client operator: only their devices, only changes that keep them theirs.
	for email, role := range map[string]string{"it@acme.com": api.RoleOperator, "ceo@acme.com": api.RoleViewer} {
		call(t, e.ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: role, Scope: acme, Password: "long-password-1"}, nil)
	}
	login := func(email string) string {
		var lr api.LoginResponse
		call(t, e.ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr)
		return lr.Token
	}
	client, viewer := login("it@acme.com"), login("ceo@acme.com")
	cb, code := bulk(client, api.BulkRequest{Operation: "wake", All: true, DryRun: true})
	if code != 200 || cb.Matched != 2 {
		t.Fatalf("client all: %d %+v", code, cb)
	}
	move, _ := bulk(client, api.BulkRequest{Operation: "set_labels", All: true, Labels: map[string]string{"customer": "globex"}, DryRun: true})
	if len(move.Applied) != 0 || len(move.Skipped) != 2 || !strings.Contains(move.Skipped[0].Reason, "out of the devices you can manage") {
		t.Fatalf("client moved devices: %+v", move)
	}
	for _, op := range []api.BulkRequest{{Operation: "exec", Args: []string{"id"}}, {Operation: "lifecycle", State: "repair"}} {
		op.All, op.DryRun = true, true
		if _, code := bulk(client, op); code != 403 {
			t.Errorf("client %s: %d", op.Operation, code)
		}
	}
	if _, code := bulk(viewer, api.BulkRequest{Operation: "wake", All: true, DryRun: true}); code != 403 {
		t.Fatalf("viewer bulk: %d", code)
	}
	cw, code := bulk(client, api.BulkRequest{Operation: "restart-agent", All: true, Expect: intp(2)})
	if code != 201 || len(cw.Applied) != 2 {
		t.Fatalf("client run: %d %+v", code, cw)
	}
	// Clients see their own bulk operations; fleet staff see all.
	var list []api.BulkOperation
	call(t, e.ts, client, "GET", "/api/v1/bulk", nil, &list)
	if len(list) != 1 || list[0].ID != cw.ID {
		t.Fatalf("client list: %+v", list)
	}
	if code := call(t, e.ts, client, "GET", "/api/v1/bulk/"+b.ID, nil, nil); code != 404 {
		t.Fatalf("client read vendor bulk: %d", code)
	}
	call(t, e.ts, admin, "GET", "/api/v1/bulk", nil, &list)
	if len(list) != 6 || list[0].ID != cw.ID {
		t.Fatalf("admin list: %d", len(list))
	}

	var evs []api.Event
	call(t, e.ts, admin, "GET", "/api/v1/events?limit=500", nil, &evs)
	found := false
	for _, ev := range evs {
		found = found || (ev.Type == "bulk.reboot" && strings.Contains(ev.Message, "on 2 devices") && strings.Contains(ev.Message, b.ID))
	}
	if !found {
		t.Fatal("no bulk event")
	}
}
