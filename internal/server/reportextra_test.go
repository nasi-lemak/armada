package server

import (
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestReportSoftwareAndDisconnections(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	keys := map[string]string{}
	for _, n := range []string{"till-1", "till-2"} {
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: n, Info: api.DeviceInfo{OS: "linux"}}, &er)
		keys[n] = er.DeviceKey
	}
	upload(t, ts, "kiosk", "1.0.0")
	upload(t, ts, "kiosk", "2.0.0")
	beat := func(n, ver string, uptime int64, conn *api.ConnectivityReport) {
		call(t, ts, keys[n], "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
			Metrics: api.Metrics{UptimeSeconds: uptime}, Packages: map[string]string{"kiosk": ver}, Connectivity: conn}, nil)
	}
	beat("till-1", "2.0.0", 50000, nil)
	beat("till-2", "1.0.0", 50000, nil)
	clock = clock.Add(20 * time.Minute)
	since := clock.Add(-15 * time.Minute)
	beat("till-1", "2.0.0", 50000+1200, &api.ConnectivityReport{Since: &since, FailedAttempts: 9, LastError: "no route to host"})
	beat("till-2", "1.0.0", 60, nil) // restarted

	q := "/api/v1/reports?from=2026-11-10&to=2026-11-10&label=customer=acme"
	var rep api.Report
	if code := call(t, ts, admin, "GET", q, nil, &rep); code != 200 {
		t.Fatalf("report: %d", code)
	}
	if len(rep.Software) != 1 || rep.Software[0].UpToDate != 1 || len(rep.Software[0].Behind) != 1 || rep.Software[0].Behind[0].Name != "till-2" {
		t.Fatalf("software: %+v", rep.Software)
	}
	if c := rep.Connectivity; c.Outages != 2 || c.Network != 1 || c.Restart != 1 || len(c.Worst) != 2 {
		t.Fatalf("connectivity: %+v", c)
	}
	per := map[string]api.ReportDevice{}
	for _, d := range rep.Devices {
		per[d.Name] = d
	}
	if per["till-1"].NetworkDrops != 1 || per["till-2"].Restarts != 1 || per["till-2"].Disconnections != 1 {
		t.Fatalf("per device: %+v", per)
	}

	get := func(format string) string {
		req, _ := http.NewRequest("GET", ts.URL+q+"&format="+format, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	rows, err := csv.NewReader(bytes.NewReader([]byte(get("csv")))).ReadAll()
	if err != nil || rows[0][len(rows[0])-3] != "disconnections" {
		t.Fatalf("csv: %v %v", err, rows[0])
	}
	html := get("html")
	for _, want := range []string{"<h2>Software</h2>", "<h2>Disconnections</h2>", "no route to host", "till-2 <span class=\"muted mono\">1.0.0</span>"} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
}
