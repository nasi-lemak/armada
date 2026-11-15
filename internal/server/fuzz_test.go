package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// fuzzServer is a server with one enrolled device, for fuzzing what
// devices send. Requests go straight to the handler, so a panic fails the
// fuzz run instead of being caught by net/http.
func fuzzServer(f *testing.F) (*Server, http.Handler, string) {
	cfg := DefaultConfig()
	cfg.DataDir = f.TempDir()
	cfg.AdminTokens = []string{admin}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { s.Close() })
	h := s.Handler()
	do := func(token, method, path string, in, out any) int {
		b, _ := json.Marshal(in)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if out != nil {
			json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec.Code
	}
	var tok api.EnrollmentToken
	do(admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	var er api.EnrollResponse
	if code := do("", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "kiosk", Info: api.DeviceInfo{OS: "linux"}}, &er); code != 200 {
		f.Fatalf("enroll: %d", code)
	}
	return s, h, er.DeviceKey
}

func heartbeatSeeds() [][]byte {
	v := 50.0
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	full := api.HeartbeatRequest{
		Info:     api.DeviceInfo{OS: "linux", Arch: "arm64", Hostname: "kiosk", Serial: "SN1", IPs: []string{"10.0.0.2"}},
		Metrics:  api.Metrics{NumCPU: 4, Load1: 0.5, UptimeSeconds: 100, MemTotalBytes: 1 << 30, MemAvailableBytes: 1 << 29},
		Packages: map[string]string{"kiosk": "1.0.0", api.AgentPackage: "1.2.3"},
		Health:   []api.CheckResult{{Name: "disk /", Kind: "disk_space", Status: api.HealthWarning, Value: &v, Unit: "%", CheckedAt: now}},
		Certs: []api.CertReport{{Target: "mosquitto", Info: &api.CertInfo{Subject: "CN=x", NotBefore: since, NotAfter: now.Add(30 * 24 * time.Hour),
			DNSNames: []string{"broker.example"}}}},
		Connectivity: &api.ConnectivityReport{Since: &since, FailedAttempts: 12, LastError: "dial tcp: timeout"},
		Backlog:      []api.BacklogSample{{At: since, Metrics: api.Metrics{Load1: 1}, Health: []api.CheckResult{{Name: "cpu", Status: api.HealthOK, CheckedAt: since}}}},
		Peripherals:  []api.PeripheralReport{{Address: "aa:bb:cc:dd:ee:ff", Name: "sensor", Model: "s1", Protocol: "mcumgr", FirmwareVersion: "1.0", RSSI: -60, BatteryPercent: 80}},
		Config:       []api.ConfigState{},
	}
	b, _ := json.Marshal(full)
	return [][]byte{b, []byte(`{}`), []byte(`{"info":{"os":"android"},"backlog":[{"at":"1970-01-01T00:00:00Z"}]}`),
		[]byte(`{"connectivity":{"since":"9999-12-31T23:59:59Z","failed_attempts":-1}}`), []byte(`{"packages":null,"health":null}`)}
}

// FuzzHeartbeat posts arbitrary check-ins as an enrolled device. Nothing a
// device sends may crash the server or make it fail internally.
func FuzzHeartbeat(f *testing.F) {
	_, h, key := fuzzServer(f)
	for _, b := range heartbeatSeeds() {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest("POST", "/api/v1/agent/heartbeat", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("heartbeat %q: %d %s", body, rec.Code, rec.Body)
		}
		// The device stays readable on every page that shows it.
		for _, p := range []string{"/api/v1/devices", "/api/v1/summary", "/api/v1/insights", "/api/v1/certificates"} {
			req := httptest.NewRequest("GET", p, nil)
			req.Header.Set("Authorization", "Bearer "+admin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("after heartbeat %q, GET %s: %d %s", body, p, rec.Code, rec.Body)
			}
		}
	})
}

func FuzzParseImport(f *testing.F) {
	f.Add(IMPORT_SAMPLE)
	f.Add("serial\n\"unterminated")
	f.Add("\ufeffserial,labels\nA1,\"a=b;c=d\"\n")
	f.Add("name\nx\n")
	f.Fuzz(func(t *testing.T, csv string) {
		recs, err := parseImport(strings.NewReader(csv))
		if err != nil {
			return
		}
		for _, r := range recs {
			_ = r
		}
	})
}

const IMPORT_SAMPLE = "serial,name,channel,labels,asset_tag,location,contact,notes\nSN1,kiosk-1,stable,customer=acme;site=hq,A-1,Lobby,Ann,new\n"

// FuzzParseLabelList: what parses prints back to the same labels.
func FuzzParseLabelList(f *testing.F) {
	for _, s := range []string{"customer=acme;site=hq", "a=b,c=d", "=x", "a=", "a==b", " a = b ", "a=b\nc=d", ";;"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := parseLabelList(s)
		if err != nil {
			return
		}
		for k := range m {
			if k == "" {
				t.Fatalf("%q gave an empty label key", s)
			}
		}
	})
}

func FuzzParseTimelineCursor(f *testing.F) {
	for _, s := range []string{"", "1700000000000.5.3", "x", "...", "-1.-1.-1", "99999999999999999999.1.1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := parseTimelineCursor(s)
		if err == nil && (c.skip < 0 || c.skip > maxTimelineSkip) {
			t.Fatalf("%q gave skip %d", s, c.skip)
		}
	})
}

func FuzzParseEventTime(f *testing.F) {
	for _, s := range []string{"2026-01-02", "2026-01-02T03:04:05Z", "2026-01-02 03:04", "", "0000-00-00", "9999-12-31"} {
		f.Add(s, true)
	}
	loc, _ := time.LoadLocation("Asia/Singapore")
	f.Fuzz(func(t *testing.T, s string, upper bool) {
		_, _ = parseEventTime(s, loc, upper)
		_, _ = parseEventTime(s, time.UTC, upper)
	})
}

// FuzzVerifySnapshot: a site report is checked before anything is trusted.
func FuzzVerifySnapshot(f *testing.F) {
	f.Add([]byte(`{"public_key":"","fingerprint":""}`), "")
	f.Add([]byte(`{"public_key":"AAAA","fingerprint":"x","generated_at":"2026-01-01T00:00:00Z"}`), "c2ln")
	f.Add([]byte(`null`), "")
	f.Fuzz(func(t *testing.T, snap []byte, sig string) {
		_, _ = verifySnapshot(&api.SignedSnapshot{Snapshot: snap, Signature: sig})
	})
}

// FuzzPublicEndpoints sends arbitrary bodies to every endpoint reachable
// without signing in. None may crash or fail internally.
func FuzzPublicEndpoints(f *testing.F) {
	_, h, _ := fuzzServer(f)
	paths := []string{"/api/v1/agent/enroll", "/api/v1/agent/challenge", "/api/v1/auth/login", "/api/v1/auth/2fa",
		"/api/v1/auth/passkey/begin", "/api/v1/auth/passkey/finish", "/api/v1/auth/2fa/passkey/begin", "/api/v1/auth/2fa/passkey/finish",
		"/api/v1/auth/sso/exchange", "/api/v1/sites/report", "/api/v1/android/provisioning"}
	for i := range paths {
		f.Add(uint8(i), []byte(`{}`), "")
	}
	f.Add(uint8(0), []byte(`{"token":"et_x","name":"a","info":{"os":"android"},"attestation":["AAAA"]}`), "")
	f.Add(uint8(2), []byte(`{"email":"a@b.c","password":"x","code":"123456"}`), "")
	f.Add(uint8(3), []byte(`{"challenge":"x","code":"000000"}`), "")
	f.Add(uint8(9), []byte(`{"snapshot":{"public_key":"AAAA"},"signature":"AAAA"}`), "site_abc")
	f.Fuzz(func(t *testing.T, which uint8, body []byte, token string) {
		path := paths[int(which)%len(paths)]
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		if token != "" && !strings.ContainsAny(token, "\r\n\x00") {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code >= 500 {
			t.Fatalf("POST %s %q: %d %s", path, body, rec.Code, rec.Body)
		}
	})
}
