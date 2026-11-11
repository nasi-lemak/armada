package server

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestPushHub(t *testing.T) {
	h := newPushHub()
	if h.wake("d1") {
		t.Fatal("woke a device with no connection")
	}
	if _, ready := h.subscribe("d1"); !ready {
		t.Fatal("pending wake not delivered on reconnect")
	}
	w1, _ := h.subscribe("d1")
	w2, _ := h.subscribe("d1") // agent restarted: the newer wait wins
	select {
	case <-w1.kicked:
	default:
		t.Fatal("old wait not released")
	}
	if !h.connected("d1") || !h.wake("d1") {
		t.Fatal("wake on connected device")
	}
	select {
	case <-w2.wake:
	default:
		t.Fatal("waiter not woken")
	}
	h.unsubscribe("d1", w1) // stale unsubscribe must not drop a newer waiter
	w3, _ := h.subscribe("d1")
	h.unsubscribe("d1", w2)
	if !h.connected("d1") {
		t.Fatal("stale unsubscribe removed the live waiter")
	}
	h.unsubscribe("d1", w3)
	h.wake("d2")
	h.checkedIn("d2") // heartbeat arrived anyway: no extra wake
	if _, ready := h.subscribe("d2"); ready {
		t.Fatal("stale pending wake")
	}
}

func TestLogsActionValidation(t *testing.T) {
	s, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "d", Info: api.DeviceInfo{OS: "linux"}}, &er)
	path := "/api/v1/devices/" + er.DeviceID + "/actions"
	for _, bad := range [][]string{
		nil, {"syslog"}, {"file"}, {"file", "name=../../etc/shadow"}, {"journal", "lines=0"},
		{"journal", "since=forever"}, {"journal", "unit=a b"}, {"journal", "path=/etc"},
	} {
		if code := call(t, ts, admin, "POST", path, api.CreateActionRequest{Type: api.ActionLogs, Args: bad}, nil); code != 400 {
			t.Errorf("accepted logs %v: %d", bad, code)
		}
	}
	for _, good := range [][]string{
		{"journal"}, {"journal", "unit=nginx.service", "lines=500", "since=2h", "grep=timeout"}, {"file", "name=app.log"},
	} {
		if code := call(t, ts, admin, "POST", path, api.CreateActionRequest{Type: api.ActionLogs, Args: good}, nil); code != 201 {
			t.Errorf("rejected logs %v: %d", good, code)
		}
	}
	if code := call(t, ts, admin, "POST", path, api.CreateActionRequest{Type: api.ActionLock}, nil); code != 400 {
		t.Errorf("lock on a Linux device: %d", code)
	}
	var atok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &atok)
	s.cfg.Android.RequireAttestation = false
	var ar api.EnrollResponse
	if code := call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: atok.Token, Info: api.DeviceInfo{OS: "android"}}, &ar); code != 200 {
		t.Fatalf("android enroll: %d", code)
	}
	apath := "/api/v1/devices/" + ar.DeviceID + "/actions"
	for _, args := range [][]string{nil, {"confirm=yes"}, {"confirm=" + er.DeviceID}} {
		if code := call(t, ts, admin, "POST", apath, api.CreateActionRequest{Type: api.ActionWipe, Args: args}, nil); code != 400 {
			t.Errorf("wipe with %v: %d", args, code)
		}
	}
	if code := call(t, ts, admin, "POST", apath, api.CreateActionRequest{Type: api.ActionWipe, Args: []string{"confirm=" + ar.DeviceID}}, nil); code != 201 {
		t.Errorf("confirmed wipe: %d", code)
	}
		var wr api.WakeResponse
	if code := call(t, ts, admin, "POST", "/api/v1/devices/"+er.DeviceID+"/wake", nil, &wr); code != 200 || wr.Connected {
		t.Fatalf("wake offline device: %d %+v", code, wr)
	}
}
