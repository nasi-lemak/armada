package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// callView is call with the dashboard's customer/project view header.
func callView(t *testing.T, ts *httptest.Server, token, view, method, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+token)
	if view != "" {
		req.Header.Set("X-Armada-View", view)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode < 300 {
		json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestViewAndInsights(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	type dev struct{ id, key string }
	enroll := func(name string, labels map[string]string) dev {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: labels}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux", Arch: "arm64"}}, &er)
		return dev{er.DeviceID, er.DeviceKey}
	}
	beat := func(d dev, kiosk string, uptime int64, conn *api.ConnectivityReport, certs []api.CertReport) {
		t.Helper()
		req := api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Arch: "arm64"}, Metrics: api.Metrics{UptimeSeconds: uptime},
			Packages: map[string]string{"kiosk": kiosk}, Connectivity: conn, Certs: certs}
		if code := call(t, ts, d.key, "POST", "/api/v1/agent/heartbeat", req, nil); code != 200 {
			t.Fatalf("heartbeat: %d", code)
		}
	}
	a1 := enroll("acme-till-1", map[string]string{"customer": "acme", "project": "tills"})
	a2 := enroll("acme-till-2", map[string]string{"customer": "acme", "project": "tills"})
	a3 := enroll("acme-kiosk", map[string]string{"customer": "acme", "project": "kiosks"})
	g1 := enroll("globex-1", map[string]string{"customer": "globex"})
	upload(t, ts, "kiosk", "1.0.0")
	rel2 := upload(t, ts, "kiosk", "2.0.0")
	day := 24 * time.Hour
	expiring := []api.CertReport{{Target: "mosquitto", Info: &api.CertInfo{Subject: "CN=broker", NotBefore: clock.Add(-300 * day), NotAfter: clock.Add(10 * day)}}}
	beat(a1, "2.0.0", 86400, nil, expiring)
	beat(a2, "1.0.0", 86400, nil, nil)
	beat(a3, "1.0.0", 86400, nil, nil)
	beat(g1, "1.0.0", 86400, nil, nil)

	// Groupings: what the picker offers.
	var gs []api.Grouping
	call(t, ts, admin, "GET", "/api/v1/groupings", nil, &gs)
	if len(gs) != 2 || gs[0].Key != "customer" || len(gs[0].Values) != 2 || gs[0].Values[0] != (api.GroupingValue{Value: "acme", Devices: 3}) ||
		gs[1].Key != "project" || len(gs[1].Values) != 2 {
		t.Fatalf("groupings: %+v", gs)
	}

	// Outages. a1: up all along but cut off for 10 minutes (network), with
	// the agent's error. a2: rebooted during a gap (restart). a3: a short
	// blip the agent noticed, under the offline threshold.
	clock = clock.Add(10 * time.Minute)
	since := clock.Add(-9 * time.Minute)
	beat(a1, "2.0.0", 86400+600, &api.ConnectivityReport{Since: &since, FailedAttempts: 6, LastError: "dial tcp: lookup fleet.example.com: no such host"}, expiring)
	beat(a2, "1.0.0", 120, nil, nil)
	blip := clock.Add(-40 * time.Second)
	beat(a3, "1.0.0", 86400+600, &api.ConnectivityReport{Since: &blip, FailedAttempts: 1, LastError: "i/o timeout"}, nil)
	beat(g1, "1.0.0", 86400+600, nil, nil)
	// Globex has a restart too, outside the acme view.
	clock = clock.Add(10 * time.Minute)
	beat(g1, "1.0.0", 30, nil, nil)

	var outs []api.Outage
	call(t, ts, admin, "GET", "/api/v1/devices/"+a1.id+"/outages", nil, &outs)
	if len(outs) != 1 || outs[0].Cause != api.OutageNetwork || outs[0].FailedAttempts != 6 || outs[0].Seconds != 9*60 ||
		outs[0].Detail != "dial tcp: lookup fleet.example.com: no such host" {
		t.Fatalf("a1 outages: %+v", outs)
	}
	outs = nil
	call(t, ts, admin, "GET", "/api/v1/devices/"+a2.id+"/outages", nil, &outs)
	if len(outs) != 1 || outs[0].Cause != api.OutageRestart {
		t.Fatalf("a2 outages: %+v", outs)
	}

	// Insights for the acme customer.
	in := api.Insights{}
	if code := callView(t, ts, admin, "customer=acme", "GET", "/api/v1/insights?range=7d", &in); code != 200 {
		t.Fatalf("insights: %d", code)
	}
	if in.Devices != 3 || len(in.Software) != 1 || in.Software[0].Latest != "2.0.0" || in.Software[0].UpToDate != 1 || len(in.Software[0].Behind) != 2 {
		t.Fatalf("software: %+v", in)
	}
	if c := in.Certificates; c.Total != 1 || c.Expiring != 1 || len(c.Items) != 1 || c.Items[0].DeviceName != "acme-till-1" {
		t.Fatalf("certs: %+v", c)
	}
	if c := in.Connectivity; c.Outages != 3 || c.Network != 2 || c.Restart != 1 || c.DevicesHit != 3 {
		t.Fatalf("connectivity: %+v", c)
	}
	// Project view: just the tills.
	in = api.Insights{}
	callView(t, ts, admin, "customer=acme;project=tills", "GET", "/api/v1/insights", &in)
	if in.Devices != 2 || in.Connectivity.Outages != 2 || len(in.Software[0].Behind) != 1 || in.Software[0].Behind[0].Name != "acme-till-2" {
		t.Fatalf("project insights: %+v", in)
	}
	// The whole fleet: globex's restart and its old kiosk too.
	in = api.Insights{}
	callView(t, ts, admin, "", "GET", "/api/v1/insights", &in)
	if in.Devices != 4 || in.Connectivity.Restart != 2 || len(in.Software[0].Behind) != 3 {
		t.Fatalf("fleet insights: %+v", in)
	}

	// Each straggler names the release that would bring it up to date, and
	// a rollout to just those devices shows under the project's view.
	if b := in.Software[0].Behind[0]; b.ReleaseID != rel2.ID {
		t.Fatalf("behind release: %+v", b)
	}
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel2.ID, Selector: api.Selector{DeviceIDs: []string{a3.id}}}, nil)
	var deps []api.Deployment
	callView(t, ts, admin, "project=kiosks", "GET", "/api/v1/deployments", &deps)
	if len(deps) != 1 {
		t.Fatalf("rollouts in view: %d", len(deps))
	}
	deps = nil
	callView(t, ts, admin, "customer=globex", "GET", "/api/v1/deployments", &deps)
	if len(deps) != 0 {
		t.Fatalf("rollouts outside view: %d", len(deps))
	}

	// The view narrows lists and totals everywhere, but a device link still opens.
	var devs []api.Device
	callView(t, ts, admin, "project=kiosks", "GET", "/api/v1/devices", &devs)
	if len(devs) != 1 || devs[0].ID != a3.id {
		t.Fatalf("devices in view: %+v", devs)
	}
	var sum api.FleetSummary
	callView(t, ts, admin, "customer=globex", "GET", "/api/v1/summary", &sum)
	if sum.Devices != 1 {
		t.Fatalf("summary in view: %+v", sum)
	}
	var evs []api.Event
	callView(t, ts, admin, "customer=globex", "GET", "/api/v1/events?limit=1000", &evs)
	for _, e := range evs {
		if e.DeviceID != g1.id {
			t.Fatalf("event outside view: %+v", e)
		}
	}
	if code := callView(t, ts, admin, "customer=globex", "GET", "/api/v1/devices/"+a1.id, nil); code != 200 {
		t.Fatalf("device outside view: %d", code)
	}
	if code := callView(t, ts, admin, "not a label", "GET", "/api/v1/devices", nil); code != 400 {
		t.Fatalf("bad view: %d", code)
	}

	// A client account sees only its own groupings, whatever view it asks for.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@globex.com", Role: api.RoleViewer,
		Scope: api.Selector{Labels: map[string]string{"customer": "globex"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@globex.com", Password: "long-password-1"}, &lr)
	gs = nil
	call(t, ts, lr.Token, "GET", "/api/v1/groupings", nil, &gs)
	if len(gs[0].Values) != 1 || gs[0].Values[0].Value != "globex" {
		t.Fatalf("client groupings: %+v", gs)
	}
	in = api.Insights{}
	callView(t, ts, lr.Token, "customer=acme", "GET", "/api/v1/insights", &in)
	if in.Devices != 0 {
		t.Fatalf("client widened its view: %+v", in)
	}
}
