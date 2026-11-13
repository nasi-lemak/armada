package server

import (
	"bufio"
	"encoding/csv"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// fakeSMTP accepts any number of messages and hands each one over.
func fakeSMTP(t *testing.T) (host string, port int, got chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got = make(chan string, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
				say := func(s string) { rw.WriteString(s + "\r\n"); rw.Flush() }
				say("220 test ESMTP")
				var data strings.Builder
				inData := false
				for {
					line, err := rw.ReadString('\n')
					if err != nil {
						return
					}
					if inData {
						if line == ".\r\n" {
							inData = false
							say("250 queued")
							got <- data.String()
							data.Reset()
							continue
						}
						data.WriteString(line)
						continue
					}
					switch strings.ToUpper(strings.Fields(line)[0]) {
					case "EHLO", "HELO":
						say("250 test")
					case "DATA":
						inData = true
						say("354 go ahead")
					case "QUIT":
						say("221 bye")
						return
					default:
						say("250 ok")
					}
				}
			}(c)
		}
	}()
	h, p, _ := net.SplitHostPort(l.Addr().String())
	port, _ = strconv.Atoi(p)
	return h, port, got
}

type reportEnv struct {
	s     *Server
	ts    *httptest.Server
	url   string
	clock time.Time
	ids   map[string]string
}

func (e *reportEnv) get(t *testing.T, token, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.url+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// setupReports: acme-1 and acme-2 (customer=acme), globex-1, all enrolled
// on 31 October, plus users for both clients.
func setupReports(t *testing.T, mod func(*Config)) *reportEnv {
	s, ts := setupWith(t, mod)
	e := &reportEnv{s: s, ts: ts, url: ts.URL, clock: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), ids: map[string]string{}}
	s.now = func() time.Time { return e.clock }
	for _, n := range []struct{ name, customer string }{{"acme-1", "acme"}, {"acme-2", "acme"}, {"globex-1", "globex"}} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": n.customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: n.name, Info: api.DeviceInfo{OS: "linux", Arch: "arm64"}}, &er)
		e.ids[n.name] = er.DeviceID
	}
	for email, g := range map[string]api.Grant{
		"it@acme.com":   {Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}},
		"ceo@acme.com":  {Role: api.RoleViewer, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}},
		"it@globex.com": {Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "globex"}}},
	} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: email, Role: g.Role, Scope: g.Scope, Password: "long-password-1"}, nil)
	}
	return e
}

// login signs a user in at the current (test) time: sessions expire as the clock moves.
func (e *reportEnv) login(t *testing.T, email string) string {
	t.Helper()
	var lr api.LoginResponse
	if code := call(t, e.ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: email, Password: "long-password-1"}, &lr); code != 200 {
		t.Fatalf("login %s: %d", email, code)
	}
	return lr.Token
}

func TestReportContents(t *testing.T) {
	e := setupReports(t, nil)
	ctx := t.Context()
	a1, a2, g1 := e.ids["acme-1"], e.ids["acme-2"], e.ids["globex-1"]
	nov := func(day, hour, min int) time.Time { return time.Date(2026, 11, day, hour, min, 0, 0, time.UTC) }

	// History: acme-1 online for all of 1–14 November, acme-2 for the first
	// week only, sampled a little faster than every 5 minutes as the server does.
	var samples []store.Sample
	for at := nov(1, 0, 0); at.Before(nov(15, 0, 0)); at = at.Add(270 * time.Second) {
		samples = append(samples, store.Sample{DeviceID: a1, Name: "online", Kind: "connectivity", At: at, Status: api.HealthOK})
		if at.Before(nov(8, 0, 0)) {
			samples = append(samples, store.Sample{DeviceID: a2, Name: "online", Kind: "connectivity", At: at, Status: api.HealthOK})
		}
	}
	if err := e.s.store.AddSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}
	// Roll up the first part, so the report reads both hourly and raw history.
	if err := e.s.store.CompactHistory(ctx, nov(10, 0, 0), 100*24*time.Hour, 365*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	// Alerts: one on acme-1 in the period, one before it, one on globex,
	// one about a rollout.
	resolved := nov(3, 10, 30)
	for _, a := range []*api.Alert{
		{ID: "alr_1", RuleName: "Offline", Condition: api.ConditionDeviceOffline, Key: "k1", Status: api.AlertResolved, Severity: api.HealthCritical,
			Summary: "acme-1 offline", DeviceID: a1, StartedAt: nov(3, 10, 0), ResolvedAt: &resolved},
		{ID: "alr_2", RuleName: "Offline", Key: "k2", Status: api.AlertResolved, Severity: api.HealthCritical, DeviceID: a1, StartedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)},
		{ID: "alr_3", RuleName: "Disk", Key: "k3", Status: api.AlertFiring, Severity: api.HealthWarning, DeviceID: g1, StartedAt: nov(4, 0, 0)},
		{ID: "alr_4", RuleName: "Update failed", Key: "k4", Status: api.AlertFiring, Severity: api.HealthWarning, DeviceID: a2, DeploymentID: "dep_x", StartedAt: nov(4, 0, 0)},
	} {
		if err := e.s.store.PutAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	// Updates: acme-1 installed app 2.0.0, acme-2 failed it.
	ts := e.ts
	rel := upload(t, ts, "app", "2.0.0")
	var dep api.Deployment
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID}, &dep)
	for id, st := range map[string]string{a1: api.UpdateSucceeded, a2: api.UpdateFailed} {
		u := &api.DeviceUpdate{DeploymentID: dep.ID, DeviceID: id, ReleaseID: rel.ID, Package: "app", Version: "2.0.0", Status: st, CreatedAt: nov(5, 0, 0), UpdatedAt: nov(5, 1, 0)}
		if err := e.s.store.UpsertDeviceUpdate(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	// Lifecycle changes and maintenance during the period.
	e.clock = nov(9, 9, 0)
	call(t, ts, admin, "POST", "/api/v1/devices/"+a2+"/lifecycle", api.LifecycleRequest{State: "repair", Note: "RMA 12"}, nil)
	e.clock = nov(9, 15, 0)
	call(t, ts, admin, "POST", "/api/v1/devices/"+a2+"/lifecycle", api.LifecycleRequest{State: "active"}, nil)
	ends := nov(9, 17, 0)
	call(t, ts, admin, "POST", "/api/v1/maintenance", api.Maintenance{Name: "Screen swap", Selector: api.Selector{DeviceIDs: []string{a1}}, EndsAt: &ends}, nil)
	call(t, ts, admin, "PATCH", "/api/v1/devices/"+a1, map[string]any{"name": "=1+1 <b>Kiosk</b>"}, nil)

	e.clock = nov(15, 12, 0)
	ceo := e.login(t, "ceo@acme.com")
	const q = "/api/v1/reports?from=2026-11-01&to=2026-11-14"
	var rep api.Report
	code := call(t, ts, ceo, "GET", q, nil, &rep)
	if code != 200 || rep.Title != "customer=acme" || len(rep.Devices) != 2 {
		t.Fatalf("client report: %d %q %d devices", code, rep.Title, len(rep.Devices))
	}
	byID := map[string]api.ReportDevice{}
	for _, d := range rep.Devices {
		byID[d.ID] = d
	}
	if a := byID[a1].Availability; a == nil || *a != 100 {
		t.Fatalf("acme-1 availability: %v", a)
	}
	if a := byID[a2].Availability; a == nil || *a != 50 || *byID[a2].DowntimeMinutes != 7*24*60 {
		t.Fatalf("acme-2 availability: %v downtime %v", a, byID[a2].DowntimeMinutes)
	}
	if rep.Devices[0].ID != a2 {
		t.Fatal("least available device should come first")
	}
	sum := rep.Summary
	if sum.Devices != 2 || sum.Availability == nil || *sum.Availability != 75 || sum.BelowTarget != 1 {
		t.Fatalf("summary availability: %+v", sum)
	}
	if sum.AlertsCritical != 1 || sum.AlertsWarning != 0 || len(rep.Alerts) != 1 || *rep.Alerts[0].DurationMinutes != 30 || *sum.MeanTimeToResolveMinutes != 30 {
		t.Fatalf("alerts: %+v %+v", sum, rep.Alerts)
	}
	if sum.UpdatesSucceeded != 1 || sum.UpdatesFailed != 1 || len(rep.Updates) != 1 || rep.Updates[0].Name != "app 2.0.0" || rep.Updates[0].Failed != 1 {
		t.Fatalf("updates: %+v %+v", sum, rep.Updates)
	}
	if len(rep.Changes) != 2 || !strings.Contains(rep.Changes[0].Message, "RMA 12") || sum.MaintenanceWindows != 1 || rep.Maintenance[0].Name != "Screen swap" {
		t.Fatalf("changes and maintenance: %+v %+v", rep.Changes, rep.Maintenance)
	}

	// Fleet staff: any client, or the whole fleet.
	var fleet api.Report
	call(t, ts, admin, "GET", q, nil, &fleet)
	if fleet.Title != "Whole fleet" || len(fleet.Devices) != 3 || fleet.Summary.AlertsWarning != 1 {
		t.Fatalf("fleet report: %q %d %+v", fleet.Title, len(fleet.Devices), fleet.Summary)
	}
	var other api.Report
	call(t, ts, ceo, "GET", q+"&label=customer=globex", nil, &other)
	if len(other.Devices) != 0 {
		t.Fatalf("client reported on another client's devices: %+v", other.Devices)
	}

	// CSV and printable HTML.
	code, body := e.get(t, ceo, q+"&format=csv")
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if code != 200 || err != nil || len(rows) != 3 || rows[1][4] != "50.00" {
		t.Fatalf("csv: %d %v %q", code, err, body)
	}
	if !strings.HasPrefix(rows[2][1], "'=1+1") {
		t.Fatalf("csv formula not neutralised: %q", rows[2][1])
	}
	code, body = e.get(t, ceo, q+"&format=html")
	html := string(body)
	if code != 200 || !strings.Contains(html, "<h1>customer=acme</h1>") || !strings.Contains(html, "&lt;b&gt;Kiosk") || strings.Contains(html, "<b>Kiosk") {
		t.Fatalf("html: %d %s", code, html)
	}

	// Validation.
	for _, bad := range []string{"period=fortnight", "from=2026-11-10&to=2026-11-01", "from=2026-01-01&to=2026-12-31",
		"from=nope&to=2026-11-01", "tz=Mars/Base", "from=2026-11-01&to=2026-11-14&format=xml", "label=nokey"} {
		if c, _ := e.get(t, admin, "/api/v1/reports?"+bad); c != 400 {
			t.Errorf("%s: %d", bad, c)
		}
	}
}

func TestReportPeriods(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Singapore")
	now := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC) // Friday 10:00 in Singapore
	for _, c := range []struct{ name, from, to string }{
		{"last-month", "2026-10-01T00:00:00+08:00", "2026-11-01T00:00:00+08:00"},
		{"last-week", "2026-11-02T00:00:00+08:00", "2026-11-09T00:00:00+08:00"},
		{"last-7d", "2026-11-06T00:00:00+08:00", "2026-11-13T00:00:00+08:00"},
		{"this-month", "2026-11-01T00:00:00+08:00", "2026-11-13T10:00:00+08:00"},
	} {
		f, to, err := reportPeriod(c.name, now, loc)
		wf, _ := time.Parse(time.RFC3339, c.from)
		wt, _ := time.Parse(time.RFC3339, c.to)
		if err != nil || !f.Equal(wf) || !to.Equal(wt) {
			t.Errorf("%s: %s – %s (%v)", c.name, f, to, err)
		}
	}
}

func TestScheduledReports(t *testing.T) {
	host, port, mails := fakeSMTP(t)
	e := setupReports(t, func(c *Config) {
		c.SMTPRelay = api.EmailConfig{Host: host, Port: port, From: "reports@vendor.example"}
		c.ReportBrand = "Vendor Co"
	})
	ts := e.ts
	e.clock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	acmeIT, acmeCEO, globexIT := e.login(t, "it@acme.com"), e.login(t, "ceo@acme.com"), e.login(t, "it@globex.com")
	mk := func(token string, rs api.ReportSchedule) (api.ReportSchedule, int) {
		var out api.ReportSchedule
		return out, call(t, ts, token, "POST", "/api/v1/report-schedules", rs, &out)
	}
	for name, c := range map[string]struct {
		token string
		rs    api.ReportSchedule
		want  int
	}{
		"viewer":         {acmeCEO, api.ReportSchedule{Period: "monthly", Recipients: []string{"a@acme.com"}}, 403},
		"other client":   {acmeIT, api.ReportSchedule{Period: "monthly", Recipients: []string{"a@acme.com"}, ClientScope: api.Selector{Labels: map[string]string{"customer": "globex"}}}, 403},
		"bad period":     {acmeIT, api.ReportSchedule{Period: "daily", Recipients: []string{"a@acme.com"}}, 400},
		"no recipients":  {acmeIT, api.ReportSchedule{Period: "monthly"}, 400},
		"bad recipient":  {acmeIT, api.ReportSchedule{Period: "monthly", Recipients: []string{"not an address"}}, 400},
		"bad time zone":  {acmeIT, api.ReportSchedule{Period: "monthly", Recipients: []string{"a@acme.com"}, TimeZone: "Nowhere/Land"}, 400},
		"fleet by admin": {admin, api.ReportSchedule{Period: "weekly", Recipients: []string{"ops@vendor.example"}}, 201},
	} {
		if _, code := mk(c.token, c.rs); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	rs, code := mk(acmeIT, api.ReportSchedule{Period: "monthly", Recipients: []string{"IT Team <it@acme.com>"}, TimeZone: "UTC"})
	if code != 201 || rs.ClientScope.Labels["customer"] != "acme" || rs.Recipients[0] != "it@acme.com" || rs.Name != "Monthly report: customer=acme" {
		t.Fatalf("client schedule: %d %+v", code, rs)
	}
	var list []api.ReportSchedule
	call(t, ts, globexIT, "GET", "/api/v1/report-schedules", nil, &list)
	if len(list) != 0 {
		t.Fatalf("globex sees: %+v", list)
	}
	if code := call(t, ts, globexIT, "DELETE", "/api/v1/report-schedules/"+rs.ID, nil, nil); code != 404 {
		t.Fatalf("globex deleted acme's schedule: %d", code)
	}

	tick := func() {
		t.Helper()
		if err := e.s.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	noMail := func() {
		t.Helper()
		select {
		case m := <-mails:
			t.Fatalf("unexpected email: %.300s", m)
		case <-time.After(200 * time.Millisecond):
		}
	}
	// October ended before the schedule existed; November isn't over.
	tick()
	noMail()
	next := func() string {
		t.Helper()
		select {
		case m := <-mails:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("no report email")
		}
		return ""
	}
	// 00:30 on 1 December: the vendor's weekly report (23–29 November) is
	// due; the monthly one waits until an hour after the month ends.
	e.clock = time.Date(2026, 12, 1, 0, 30, 0, 0, time.UTC)
	tick()
	if w := next(); !strings.Contains(w, "To: ops@vendor.example") || !strings.Contains(w, "Weekly_report:_Whole_fleet,_23_Nov") {
		t.Fatalf("weekly: %.400s", w)
	}
	noMail()
	e.clock = time.Date(2026, 12, 1, 1, 5, 0, 0, time.UTC)
	tick()
	m := next()
	if !strings.Contains(m, "To: it@acme.com") {
		t.Fatalf("monthly: %.300s", m)
	}
	if !strings.Contains(m, "Subject: =?utf-8?q?[Vendor_Co]_Monthly_report:_customer=3Dacme,_November_2026?=") &&
		!strings.Contains(m, "Monthly report: customer=acme, November 2026") {
		t.Fatalf("subject: %.500s", m)
	}
	if !strings.Contains(m, "Content-Type: text/html") || !strings.Contains(m, `attachment; filename="report-customer-acme-2026-11-01.csv"`) {
		t.Fatalf("parts: %.800s", m)
	}
	tick()
	noMail() // once per period
	acmeIT = e.login(t, "it@acme.com")
	call(t, ts, acmeIT, "GET", "/api/v1/report-schedules", nil, &list)
	if len(list) != 1 || list[0].LastPeriod != "2026-11-01" || list[0].LastSentAt == nil {
		t.Fatalf("after sending: %+v", list)
	}

	// Send now; not twice in a row.
	e.clock = e.clock.Add(10 * time.Minute)
	acmeIT, acmeCEO = e.login(t, "it@acme.com"), e.login(t, "ceo@acme.com")
	if code := call(t, ts, acmeIT, "POST", "/api/v1/report-schedules/"+rs.ID+"/send", nil, nil); code != 200 {
		t.Fatalf("send now: %d", code)
	}
	<-mails
	if code := call(t, ts, acmeIT, "POST", "/api/v1/report-schedules/"+rs.ID+"/send", nil, nil); code != 429 {
		t.Fatalf("send again: %d", code)
	}
	if code := call(t, ts, acmeCEO, "POST", "/api/v1/report-schedules/"+rs.ID+"/send", nil, nil); code != 403 {
		t.Fatalf("viewer sent: %d", code)
	}
	if code := call(t, ts, acmeIT, "DELETE", "/api/v1/report-schedules/"+rs.ID, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}

func TestReportCountsReplacementsOnceAndNotClosedAlertsAsFixed(t *testing.T) {
	e := setupReports(t, nil)
	ts := e.ts
	nov := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "stock-1", Info: api.DeviceInfo{OS: "linux"}}, &er)
	e.clock = nov(2, 9)
	call(t, ts, admin, "POST", "/api/v1/devices/"+e.ids["acme-2"]+"/replace", api.ReplaceRequest{ReplacementID: er.DeviceID}, nil)

	fixed, closed := nov(3, 1), nov(4, 0)
	for _, a := range []*api.Alert{
		{ID: "alr_a", RuleName: "Offline", Key: "a", Status: api.AlertResolved, Severity: api.HealthCritical, DeviceID: e.ids["acme-1"], StartedAt: nov(3, 0), ResolvedAt: &fixed},
		{ID: "alr_b", RuleName: "Offline", Key: "b", Status: api.AlertResolved, Severity: api.HealthCritical, DeviceID: e.ids["acme-1"], StartedAt: nov(3, 0), ResolvedAt: &closed, ClosedOutOfService: true},
	} {
		if err := e.s.store.PutAlert(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	e.clock = nov(15, 0)
	var rep api.Report
	call(t, ts, admin, "GET", "/api/v1/reports?from=2026-11-01&to=2026-11-14", nil, &rep)
	if rep.Summary.Replacements != 1 {
		t.Fatalf("replacements: %d (changes %+v)", rep.Summary.Replacements, rep.Changes)
	}
	if rep.Summary.MeanTimeToResolveMinutes == nil || *rep.Summary.MeanTimeToResolveMinutes != 60 {
		t.Fatalf("mean time to resolve counted an out-of-service close: %v", rep.Summary.MeanTimeToResolveMinutes)
	}
	var client api.Report
	call(t, ts, admin, "GET", "/api/v1/reports?from=2026-11-01&to=2026-11-14&label=customer=acme", nil, &client)
	if client.Summary.Replacements != 1 || client.Summary.Devices != 2 {
		t.Fatalf("client replacements: %+v", client.Summary)
	}
}

func TestMeasureAvailability(t *testing.T) {
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	hour := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	full := func(except ...int) map[int64]int { // 12 samples an hour, none in the hours given
		c := map[int64]int{}
		for h := 0; h < 24; h++ {
			c[hour(h).Unix()] = 12
		}
		for _, h := range except {
			delete(c, hour(h).Unix())
		}
		return c
	}
	iv := func(from, to time.Time) []rollout.Interval { return []rollout.Interval{{Start: from, End: to}} }
	for _, c := range []struct {
		name             string
		counts           map[int64]int
		start, end       time.Time
		planned          []rollout.Interval
		avail, down, mnt float64
		measured         bool
	}{
		{"always up", full(), hour(0), hour(24), nil, 100, 0, 0, true},
		{"down 2h, no maintenance", full(3, 4), hour(0), hour(24), nil, 100 * (1 - 120.0/1440), 120, 0, true},
		{"down 2h, all in maintenance", full(3, 4), hour(0), hour(24), iv(hour(3), hour(5)), 100, 0, 120, true},
		{"down 2h, half in maintenance", full(3, 4), hour(0), hour(24), iv(hour(3), hour(4)), 100 * (1 - 60.0/1380), 60, 60, true},
		{"up during maintenance", full(), hour(0), hour(24), iv(hour(3).Add(30*time.Minute), hour(4)), 100, 0, 30, true},
		{"down 30 min in an hour with 20 min maintenance", map[int64]int{hour(0).Unix(): 6}, hour(0), hour(1), iv(hour(0), hour(0).Add(20*time.Minute)), 100 * (1 - 10.0/40), 10, 20, true},
		{"all in maintenance", nil, hour(0), hour(2), iv(hour(0), hour(2)), 0, 0, 120, false},
		// Enrolled at 10:20: a partial first hour is measured on its own.
		{"partial first hour", map[int64]int{hour(10).Unix(): 9, hour(11).Unix(): 12}, hour(10).Add(20 * time.Minute), hour(12), nil, 100, 0, 0, true},
	} {
		r := measureAvailability(c.counts, c.start, c.end, 12, c.planned)
		if r.measured != c.measured || math.Abs(r.availability-c.avail) > 0.001 || math.Abs(r.downtime-c.down) > 0.001 || math.Abs(r.maintenance-c.mnt) > 0.001 {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
}

func TestReportLeavesOutMaintenance(t *testing.T) {
	e := setupReports(t, nil) // clock: 31 October
	ts, a1 := e.ts, e.ids["acme-1"]
	nov := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	// Weekly maintenance for the client on Sundays 02:00-04:00 (1 and 8 November),
	// and an updates-only window on 5 November, which doesn't excuse downtime.
	f := false
	if code := call(t, ts, admin, "POST", "/api/v1/maintenance", api.Maintenance{Name: "Sunday reboot",
		Selector: api.Selector{Labels: map[string]string{"customer": "acme"}}, Windows: "02:00-04:00", Days: []string{"sun"}}, nil); code != 201 {
		t.Fatalf("recurring maintenance: %d", code)
	}
	from, to := nov(5, 10), nov(5, 12)
	if code := call(t, ts, admin, "POST", "/api/v1/maintenance", api.Maintenance{Name: "Updates only",
		Selector: api.Selector{DeviceIDs: []string{a1}}, StartsAt: from, EndsAt: &to, SuppressAlerts: &f}, nil); code != 201 {
		t.Fatalf("updates-only maintenance: %d", code)
	}
	// acme-1 is down during both Sunday windows and for those two hours on 5 November.
	down := []rollout.Interval{{Start: nov(1, 2), End: nov(1, 4)}, {Start: nov(8, 2), End: nov(8, 4)}, {Start: from, End: to}}
	var samples []store.Sample
	for at := nov(1, 0); at.Before(nov(15, 0)); at = at.Add(270 * time.Second) {
		off := false
		for _, d := range down {
			off = off || (!at.Before(d.Start) && at.Before(d.End))
		}
		if !off {
			samples = append(samples, store.Sample{DeviceID: a1, Name: "online", Kind: "connectivity", At: at, Status: api.HealthOK})
		}
	}
	if err := e.s.store.AddSamples(t.Context(), samples); err != nil {
		t.Fatal(err)
	}
	e.clock = nov(15, 12)
	var rep api.Report
	call(t, ts, admin, "GET", "/api/v1/reports?from=2026-11-01&to=2026-11-14&label=customer=acme", nil, &rep)
	var line api.ReportDevice
	for _, d := range rep.Devices {
		if d.ID == a1 {
			line = d
		}
	}
	want := math.Round(100*(1-120.0/(14*1440-240))*100) / 100 // 99.40
	if line.Availability == nil || *line.Availability != want || *line.DowntimeMinutes != 120 || line.MaintenanceMinutes == nil || *line.MaintenanceMinutes != 240 {
		t.Fatalf("acme-1: availability %v downtime %v maintenance %v (want %v, 120, 240)", line.Availability, line.DowntimeMinutes, line.MaintenanceMinutes, want)
	}
	if len(rep.Maintenance) != 2 {
		t.Fatalf("maintenance listed: %+v", rep.Maintenance)
	}
}
