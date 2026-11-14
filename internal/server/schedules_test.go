package server

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestScheduledActions(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC) // 02:00 on Tue 3 Nov in Singapore
	s.now = func() time.Time { return clock }
	ctx := t.Context()
	tokens := map[string]string{}
	enroll := func(name, customer string) string {
		if tokens[customer] == "" {
			var tok api.EnrollmentToken
			call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
			tokens[customer] = tok.Token
		}
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tokens[customer], Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er.DeviceID
	}
	a1, a2, g1 := enroll("acme-1", "acme"), enroll("acme-2", "acme"), enroll("globex-1", "globex")
	_ = a2

	var sa api.ScheduledAction
	if code := call(t, ts, admin, "POST", "/api/v1/scheduled-actions", api.ScheduledAction{Name: "Nightly reboot", Operation: "reboot",
		Selector: api.Selector{Labels: map[string]string{"customer": "acme"}}, At: "03:00", TimeZone: "Asia/Singapore", Enabled: true}, &sa); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if want := time.Date(2026, 11, 2, 19, 0, 0, 0, time.UTC); sa.NextRunAt == nil || !sa.NextRunAt.Equal(want) || sa.Matches != 2 ||
		sa.HoldAlertsMinutes == nil || *sa.HoldAlertsMinutes != 30 {
		t.Fatalf("created: %+v next %v", sa, sa.NextRunAt)
	}
	get := func() api.ScheduledAction {
		t.Helper()
		var all []api.ScheduledAction
		call(t, ts, admin, "GET", "/api/v1/scheduled-actions", nil, &all)
		for _, x := range all {
			if x.ID == sa.ID {
				return x
			}
		}
		t.Fatal("schedule gone")
		return api.ScheduledAction{}
	}
	if err := s.scheduleTick(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if get().LastRun != nil {
		t.Fatal("ran early")
	}
	clock = time.Date(2026, 11, 2, 19, 0, 30, 0, time.UTC)
	if err := s.scheduleTick(ctx, clock); err != nil {
		t.Fatal(err)
	}
	got := get()
	if got.LastRun == nil || got.LastRun.Applied != 2 || got.LastRun.BulkID == "" || got.LastRun.Error != "" ||
		!got.NextRunAt.Equal(time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("after run: %+v %+v", got, got.LastRun)
	}
	// The reboots are queued, and the devices' alerts are held.
	var det struct {
		Device  *api.Device   `json:"device"`
		Actions []*api.Action `json:"actions"`
	}
	call(t, ts, admin, "GET", "/api/v1/devices/"+a1, nil, &det)
	if len(det.Actions) != 1 || det.Actions[0].Type != "reboot" || det.Actions[0].BatchID != got.LastRun.BulkID {
		t.Fatalf("a1 actions: %+v", det.Actions)
	}
	if det.Device.Maintenance == nil || !det.Device.Maintenance.Alerts {
		t.Fatalf("a1 not in maintenance: %+v", det.Device.Maintenance)
	}
	det.Device, det.Actions = nil, nil
	call(t, ts, admin, "GET", "/api/v1/devices/"+g1, nil, &det)
	if len(det.Actions) != 0 || det.Device.Maintenance != nil {
		t.Fatalf("globex touched: %+v %+v", det.Actions, det.Device.Maintenance)
	}

	// Down for two days: the run is recorded as missed, not run late.
	clock = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err := s.scheduleTick(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if got = get(); got.LastRun == nil || !got.LastRun.Missed || !got.NextRunAt.Equal(time.Date(2026, 11, 5, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("missed: %+v %+v", got.LastRun, got.NextRunAt)
	}

	// Paused: no next run.
	got.Enabled = false
	var paused api.ScheduledAction
	if code := call(t, ts, admin, "PUT", "/api/v1/scheduled-actions/"+sa.ID, got, &paused); code != 200 || paused.NextRunAt != nil || paused.Enabled {
		t.Fatalf("pause: %d %+v", code, paused.NextRunAt)
	}
	if err := s.scheduleTick(ctx, clock.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g := get(); !g.LastRun.Missed || g.NextRunAt != nil {
		t.Fatalf("paused schedule ran: %+v", g.LastRun)
	}

	// Days: Saturdays only.
	var sat api.ScheduledAction
	call(t, ts, admin, "POST", "/api/v1/scheduled-actions", api.ScheduledAction{Name: "Weekly", Operation: "restart-agent", All: true,
		At: "23:30", Days: []string{"Saturday"}, Enabled: true}, &sat)
	if sat.NextRunAt == nil || sat.NextRunAt.Weekday() != time.Saturday || sat.Days[0] != "sat" || sat.NextRunAt.Hour() != 23 {
		t.Fatalf("saturday: %+v", sat)
	}

	for name, bad := range map[string]api.ScheduledAction{
		"no devices": {Name: "x", Operation: "reboot", At: "03:00"},
		"bad time":   {Name: "x", Operation: "reboot", At: "25:00", All: true},
		"bad day":    {Name: "x", Operation: "reboot", At: "03:00", All: true, Days: []string{"funday"}},
		"wipe":       {Name: "x", Operation: "wipe", At: "03:00", All: true},
		"exec bare":  {Name: "x", Operation: "exec", At: "03:00", All: true},
		"no name":    {Operation: "reboot", At: "03:00", All: true},
	} {
		if code := call(t, ts, admin, "POST", "/api/v1/scheduled-actions", bad, nil); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}

	// A client operator's schedule only ever touches that client's devices,
	// even with all=true, and the client sees its runs.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)
	var cs api.ScheduledAction
	if code := call(t, ts, lr.Token, "POST", "/api/v1/scheduled-actions", api.ScheduledAction{Name: "Acme wake", Operation: "wake", All: true,
		At: "06:00", Enabled: true}, &cs); code != 201 || cs.ClientScope.Labels["customer"] != "acme" || cs.Matches != 2 {
		t.Fatalf("client create: %d %+v", code, cs)
	}
	var ran api.ScheduledAction
	if code := call(t, ts, lr.Token, "POST", "/api/v1/scheduled-actions/"+cs.ID+"/run", nil, &ran); code != 200 || ran.LastRun == nil ||
		!ran.LastRun.Manual || ran.LastRun.Applied != 2 {
		t.Fatalf("client run now: %d %+v", code, ran.LastRun)
	}
	var bulks []api.BulkOperation
	call(t, ts, lr.Token, "GET", "/api/v1/bulk", nil, &bulks)
	found := false
	for _, b := range bulks {
		found = found || b.ID == ran.LastRun.BulkID
	}
	if !found {
		t.Fatal("client can't see its schedule's bulk operation")
	}
	var mine []api.ScheduledAction
	call(t, ts, lr.Token, "GET", "/api/v1/scheduled-actions", nil, &mine)
	if len(mine) != 1 || mine[0].ID != cs.ID {
		t.Fatalf("client sees: %+v", mine)
	}
	if code := call(t, ts, lr.Token, "POST", "/api/v1/scheduled-actions", api.ScheduledAction{Name: "x", Operation: "exec", Args: []string{"true"},
		All: true, At: "03:00"}, nil); code != 403 {
		t.Fatalf("client exec: %d", code)
	}
	if code := call(t, ts, lr.Token, "DELETE", "/api/v1/scheduled-actions/"+sa.ID, nil, nil); code != 404 {
		t.Fatalf("client deleted vendor schedule: %d", code)
	}
	if code := call(t, ts, lr.Token, "DELETE", "/api/v1/scheduled-actions/"+cs.ID, nil, nil); code != 204 {
		t.Fatalf("client delete own: %d", code)
	}
}
