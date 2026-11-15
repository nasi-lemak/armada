package server

import (
	"bufio"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func getMetrics(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/metrics", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

var (
	metricLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\]|\\.)*",?)*\})? (-?[0-9.e+-]+|\+Inf|NaN)$`)
	typeLine   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (counter|gauge|histogram)$`)
)

// checkExposition parses the text format: every sample belongs to a family
// declared before it, and no family is declared twice.
func checkExposition(t *testing.T, body string) {
	t.Helper()
	declared := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# HELP "):
		case strings.HasPrefix(line, "# TYPE "):
			m := typeLine.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("bad TYPE line: %q", line)
				continue
			}
			if declared[m[1]] != "" {
				t.Errorf("family %s declared twice", m[1])
			}
			declared[m[1]] = m[2]
		default:
			m := metricLine.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("bad sample line: %q", line)
				continue
			}
			name := m[1]
			if declared[name] == "" {
				base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
				if declared[base] != "histogram" {
					t.Errorf("sample %s before its TYPE line", name)
				}
			}
		}
	}
}

func TestMetrics(t *testing.T) {
	s, ts := setupWith(t, func(c *Config) { c.MetricsToken = "prometheus-scrape-token-1" })
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "kiosk-1", Info: api.DeviceInfo{OS: "linux"}}, &er)
	for i := 0; i < 3; i++ {
		if code := call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"}}, nil); code != 200 {
			t.Fatalf("heartbeat: %d", code)
		}
	}
	call(t, ts, admin, "GET", "/api/v1/devices/"+er.DeviceID, nil, nil)
	call(t, ts, admin, "GET", "/api/v1/devices/dev_nope", nil, nil)
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.metrics.observeTick(1500000, nil)

	code, body := getMetrics(t, ts.URL, admin)
	if code != 200 {
		t.Fatalf("metrics as admin: %d %s", code, body)
	}
	checkExposition(t, body)
	for _, want := range []string{
		`armada_devices{status="online",lifecycle="active"} 1`,
		`armada_devices{status="offline",lifecycle="spare"} 0`,
		`armada_heartbeats_total 3`,
		`armada_enrollments_total 1`,
		`armada_http_requests_total{method="POST",route="/api/v1/agent/heartbeat",code="200"} 3`,
		`armada_http_requests_total{method="GET",route="/api/v1/devices/{id}",code="200"} 1`,
		`armada_http_requests_total{method="GET",route="/api/v1/devices/{id}",code="404"} 1`,
		`armada_http_request_duration_seconds_count{method="POST",route="/api/v1/agent/heartbeat"} 3`,
		`armada_http_request_duration_seconds_bucket{method="POST",route="/api/v1/agent/heartbeat",le="+Inf"} 3`,
		`armada_engine_ticks_total 1`,
		`armada_engine_tick_duration_seconds 0.0015`,
		`armada_alerts_firing{severity="critical"} 0`,
		`armada_backup_enabled 0`,
		`armada_push_connections 0`,
		`# TYPE armada_http_request_duration_seconds histogram`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics lack %q", want)
		}
	}
	// Paths are counted by pattern: a device ID never becomes a label.
	if strings.Contains(body, er.DeviceID) || strings.Contains(body, "dev_nope") {
		t.Error("a device ID appears in the metrics")
	}

	// Who may read them.
	if code, _ := getMetrics(t, ts.URL, ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code, _ := getMetrics(t, ts.URL, "wrong-token"); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, body := getMetrics(t, ts.URL, "prometheus-scrape-token-1"); code != 200 || !strings.Contains(body, "armada_heartbeats_total") {
		t.Errorf("metrics token: %d", code)
	}
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleViewer,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "acme-password-1"}, nil)
	var login api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "acme-password-1"}, &login)
	if code, _ := getMetrics(t, ts.URL, login.Token); code != 403 {
		t.Errorf("client account read fleet metrics: %d", code)
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	var b strings.Builder
	w := bufio.NewWriter(&b)
	e := &expo{w: w, seen: map[string]bool{}}
	e.gauge("x", "help", 1, "site", "a \"b\"\\c\nd")
	w.Flush()
	if want := `x{site="a \"b\"\\c\nd"} 1` + "\n"; !strings.HasSuffix(b.String(), want) {
		t.Errorf("got %q", b.String())
	}
	checkExposition(t, b.String())
}

func TestInstallScriptsServed(t *testing.T) {
	_, ts := setup(t)
	for path, want := range map[string]string{"/install.sh": "launchctl bootstrap", "/install.ps1": "-install-service"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(b), want) {
			t.Errorf("%s: %d, %d bytes", path, res.StatusCode, len(b))
		}
		if strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
			t.Errorf("%s served as HTML", path)
		}
	}
}
