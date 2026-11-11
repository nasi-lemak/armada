package server

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestParseRange(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 24 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "365d": 90 * 24 * time.Hour} {
		got, err := parseRange(in, 24*time.Hour, 90*24*time.Hour)
		if err != nil || got != want {
			t.Errorf("parseRange(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"x", "-1h", "0d", "d"} {
		if _, err := parseRange(bad, time.Hour, time.Hour); err == nil {
			t.Errorf("parseRange(%q) accepted", bad)
		}
	}
}

func TestHistoryRecordingRollupAndRetention(t *testing.T) {
	e := setupAlerts(t) // server + one device + controllable clock
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	e.clock = start
	temp := func(v float64, status string) []api.CheckResult {
		return []api.CheckResult{{Name: "temperature cpu", Kind: "temperature", Status: status, Value: &v, Unit: "°C"},
			{Name: "monitor web", Kind: "http", Status: api.HealthOK}}
	}
	// Two hours of heartbeats every minute, with a 30-minute outage in the middle
	// and the temperature running hot for 20 minutes.
	for m := 0; m < 120; m++ {
		e.clock = start.Add(time.Duration(m) * time.Minute)
		if m >= 40 && m < 70 {
			continue // offline
		}
		status := api.HealthOK
		if m >= 90 && m < 110 {
			status = api.HealthWarning
		}
		e.heartbeat(t, temp(40+float64(m)/10, status))
		e.tick(t)
	}

	var h api.DeviceHistory
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID+"/history?range=24h", nil, &h)
	series := map[string]api.HistorySeries{}
	for _, s := range h.Series {
		series[s.Name] = s
	}
	tc := series["temperature cpu"]
	// One sample per 5 minutes: 24 slots minus the 6 during the outage.
	if len(tc.Points) != 18 || !tc.Numeric || tc.Unit != "°C" || h.Resolution != 300 {
		t.Fatalf("temperature series: %d points numeric=%v unit=%q res=%d", len(tc.Points), tc.Numeric, tc.Unit, h.Resolution)
	}
	if *tc.Availability < 0.77 || *tc.Availability > 0.79 { // 14 of 18 samples ok
		t.Fatalf("temperature ok share: %v", *tc.Availability)
	}
	if on := series["online"]; on.Availability == nil || *on.Availability < 0.74 || *on.Availability > 0.76 {
		t.Fatalf("online availability: %+v", on.Availability) // 18 of 24 periods
	}
	if web := series["monitor web"]; web.Numeric || *web.Availability != 1 {
		t.Fatalf("status-only series: %+v", web)
	}
	if _, ok := series["load"]; !ok {
		t.Fatalf("metrics series missing: %v", h.Series)
	}
	if _, dup := series["memory used"]; dup {
		t.Fatal("Linux agents report memory themselves; the heartbeat fallback must not duplicate it")
	}

	// Long ranges use hourly summaries, including the current (not yet rolled up) hour.
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID+"/history?range=7d", nil, &h)
	for _, s := range h.Series {
		series[s.Name] = s
	}
	tc = series["temperature cpu"]
	if h.Resolution != 3600 || len(tc.Points) != 2 {
		t.Fatalf("hourly: res %d, %d points", h.Resolution, len(tc.Points))
	}
	first := tc.Points[0]
	if first.T != start.Unix() || *first.Min != 40 || *first.Max != 43.5 || first.S != api.HealthOK {
		t.Fatalf("first hour summary: %+v min=%v max=%v", first, *first.Min, *first.Max)
	}
	if tc.Points[1].S != api.HealthWarning {
		t.Fatalf("second hour should carry the worst status: %+v", tc.Points[1])
	}

	var fleet api.FleetHistory
	call(t, e.ts, admin, "GET", "/api/v1/history/fleet?range=24h", nil, &fleet)
	if len(fleet.Points) == 0 || fleet.Points[0].Online != 1 {
		t.Fatalf("fleet history: %+v", fleet.Points)
	}

	// After 4 days raw samples are gone but hourly summaries remain…
	e.clock = start.Add(4 * 24 * time.Hour)
	e.s.history.lastCompact = time.Time{}
	e.tick(t)
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID+"/history?range=24h", nil, &h)
	for _, s := range h.Series {
		if s.Name != "online" && len(s.Points) != 0 {
			t.Fatalf("raw samples outlived retention: %s %d", s.Name, len(s.Points))
		}
	}
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+e.devID+"/history?range=7d", nil, &h)
	for _, s := range h.Series {
		if s.Name == "temperature cpu" && len(s.Points) != 2 {
			t.Fatalf("hourly summaries lost: %d", len(s.Points))
		}
	}
	// …and after 100 days everything is gone, including the series rows.
	e.clock = start.Add(100 * 24 * time.Hour)
	e.s.history.lastCompact = time.Time{}
	e.tick(t)
	var n int
	e.s.store.DB().QueryRow(`SELECT COUNT(*) FROM series WHERE name = 'temperature cpu'`).Scan(&n)
	if n != 0 {
		t.Fatalf("series rows not cleaned up: %d", n)
	}

	// Deleting a device deletes its history.
	e.heartbeat(t, temp(50, api.HealthOK))
	call(t, e.ts, admin, "DELETE", "/api/v1/devices/"+e.devID, nil, nil)
	e.s.store.DB().QueryRow(`SELECT COUNT(*) FROM series`).Scan(&n)
	if n != 0 {
		t.Fatalf("history kept for deleted device: %d series", n)
	}
}
