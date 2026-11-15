package server

import (
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestFlakyConnectivityAlert(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Flaky", Condition: api.ConditionFlakyConnectivity, Disconnections: 3, WindowHours: 6, NotifyResolved: true})
	e.heartbeat(t, nil)
	drop := func() {
		since := e.clock.Add(-30 * time.Second)
		call(t, e.ts, e.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}, Metrics: api.Metrics{UptimeSeconds: 99999},
			Connectivity: &api.ConnectivityReport{Since: &since, FailedAttempts: 2, LastError: "i/o timeout"}}, nil)
	}
	for i := 0; i < 2; i++ {
		e.clock = e.clock.Add(time.Minute)
		drop()
	}
	e.tick(t)
	none(t, e.hook)
	e.clock = e.clock.Add(time.Minute)
	drop()
	e.tick(t)
	n := notification(t, next(t, e.hook))
	if n.Status != api.AlertFiring || n.DeviceName != "kiosk-07" || !strings.Contains(n.Summary, "3 times") || !strings.Contains(n.Summary, "i/o timeout") {
		t.Fatalf("flaky: %+v", n)
	}
	// Six hours on, the drops are outside the window: resolved.
	e.clock = e.clock.Add(7 * time.Hour)
	e.heartbeat(t, nil)
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved {
		t.Fatalf("resolved: %+v", n)
	}
}

func TestSiteOutageAlert(t *testing.T) {
	e := setupAlerts(t)
	e.rule(t, api.AlertRule{Name: "Site down", Condition: api.ConditionSiteOutage, GroupBy: "site", MinDevices: 3, MinPercent: 60, NotifyResolved: true})
	// Four devices at site=sg (kiosk-07 plus three), one at site=kl.
	var tok, tokKL api.EnrollmentToken
	call(t, e.ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"site": "sg"}}, &tok)
	call(t, e.ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"site": "kl"}}, &tokKL)
	keys := []string{e.key}
	for i, tk := range []string{tok.Token, tok.Token, tok.Token, tokKL.Token} {
		var er api.EnrollResponse
		call(t, e.ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tk, Name: "dev-" + string(rune('a'+i)), Info: api.DeviceInfo{OS: "linux"}}, &er)
		keys = append(keys, er.DeviceKey)
	}
	beat := func(ks ...string) {
		for _, k := range ks {
			call(t, e.ts, k, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, nil)
		}
	}
	beat(keys...)
	// Ten minutes on, only one sg device and the kl device still check in:
	// 3 of 4 at sg are offline (75%).
	e.clock = e.clock.Add(10 * time.Minute)
	beat(keys[3], keys[4])
	e.tick(t)
	n := notification(t, next(t, e.hook))
	if n.Status != api.AlertFiring || n.Severity != api.HealthCritical || !strings.Contains(n.Summary, "3 of 4 devices at site=sg") {
		t.Fatalf("site outage: %+v", n)
	}
	e.tick(t)
	none(t, e.hook) // one alert for the site, not one per device
	// Two come back: 1 of 4 offline, below the thresholds.
	beat(keys[0], keys[1])
	e.tick(t)
	if n := notification(t, next(t, e.hook)); n.Status != api.AlertResolved {
		t.Fatalf("resolved: %+v", n)
	}
	// A rule with a bad group key is refused.
	if code := call(t, e.ts, admin, "POST", "/api/v1/alert-rules", api.AlertRule{Name: "x", Condition: api.ConditionSiteOutage, GroupBy: "bad key!"}, nil); code != 400 {
		t.Fatalf("bad group_by: %d", code)
	}
}
