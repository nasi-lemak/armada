package server

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestHistoryCompare(t *testing.T) {
	s, ts := setup(t)
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	keys := map[string]string{}
	for _, d := range []struct{ name, site string }{{"cool", "sg"}, {"hot", "sg"}, {"warm", "kl"}} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"site": d.site}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: d.name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		keys[d.name] = er.DeviceKey
	}
	temps := map[string]float64{"cool": 40, "hot": 85, "warm": 60}
	for m := 0; m < 30; m += 5 {
		clock = clock.Add(5 * time.Minute)
		for name, key := range keys {
			v := temps[name] + float64(m)/10
			call(t, ts, key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
				Health: []api.CheckResult{{Name: "temperature cpu", Kind: "temperature", Status: api.HealthOK, Value: &v, Unit: "°C"}}}, nil)
		}
	}
	var checks []checkName
	call(t, ts, admin, "GET", "/api/v1/history/checks", nil, &checks)
	if len(checks) == 0 || checks[0].Devices != 3 {
		t.Fatalf("checks: %+v", checks)
	}
	var c CompareResponse
	call(t, ts, admin, "GET", "/api/v1/history/compare?check=temperature+cpu&range=24h", nil, &c)
	if c.Matching != 3 || len(c.Series) != 3 || c.Series[0].DeviceName != "hot" || c.Series[2].DeviceName != "cool" {
		t.Fatalf("compare order: matching %d %+v", c.Matching, names(c.Series))
	}
	call(t, ts, admin, "GET", "/api/v1/history/compare?check=temperature+cpu&label=site=sg&limit=1", nil, &c)
	if c.Matching != 2 || len(c.Series) != 1 || c.Series[0].DeviceName != "hot" {
		t.Fatalf("filtered compare: %d %+v", c.Matching, names(c.Series))
	}
	call(t, ts, admin, "GET", "/api/v1/history/compare?check=temperature+cpu&limit=99", nil, &c)
	if len(c.Series) != 3 {
		t.Fatalf("limit clamp: %d", len(c.Series))
	}
	if code := call(t, ts, admin, "GET", "/api/v1/history/compare", nil, nil); code != 400 {
		t.Fatalf("missing check: %d", code)
	}
}

func names(ss []api.HistorySeries) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, s.DeviceName)
	}
	return out
}

func TestBackupRestore(t *testing.T) {
	s, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "keeper", Info: api.DeviceInfo{OS: "linux"}}, &er)
	rel := upload(t, ts, "app", "1.0.0")

	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/backup", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("backup: %v %v", err, resp.Status)
	}
	dir := t.TempDir()
	gz, _ := gzip.NewReader(resp.Body)
	tr := tar.NewReader(gz)
	var got []string
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, hd.Name)
		os.MkdirAll(filepath.Join(dir, filepath.Dir(hd.Name)), 0o700)
		f, _ := os.Create(filepath.Join(dir, hd.Name))
		io.Copy(f, tr)
		f.Close()
	}
	resp.Body.Close()
	if len(got) != 4 {
		t.Fatalf("archive contents: %v", got)
	}
	// A server started from the restored directory has the same devices,
	// releases, artifacts and signing key.
	cfg := s.cfg
	cfg.DataDir = dir
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rts := httptest.NewServer(restored.Handler())
	defer rts.Close()
	var devs []api.Device
	call(t, rts, admin, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 1 || devs[0].Name != "keeper" {
		t.Fatalf("restored devices: %+v", devs)
	}
	if code := call(t, rts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 200 {
		t.Fatalf("device key not valid after restore: %d", code)
	}
	if restored.PublicKey() != s.PublicKey() {
		t.Fatal("signing key changed after restore")
	}
	if code := call(t, rts, admin, "GET", "/api/v1/releases/"+rel.ID+"/artifact", nil, nil); code != 200 {
		t.Fatalf("artifact missing after restore: %d", code)
	}
}
