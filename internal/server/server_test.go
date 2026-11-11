package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

const admin = "admin-secret"

func setup(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AdminTokens = []string{admin}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, token, method, path string, in, out any) int {
	t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func upload(t *testing.T, ts *httptest.Server, pkg, version string) api.Release {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("package", pkg)
	mw.WriteField("version", version)
	fw, _ := mw.CreateFormFile("file", "a.bin")
	fw.Write([]byte("payload " + version))
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/releases", &buf)
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	var res uploadResult
	json.NewDecoder(resp.Body).Decode(&res)
	return *res.Release
}

func TestAuthRequired(t *testing.T) {
	_, ts := setup(t)
	if code := call(t, ts, "", "GET", "/api/v1/devices", nil, nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code := call(t, ts, "wrong", "GET", "/api/v1/devices", nil, nil); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code := call(t, ts, "dk_nope", "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 401 {
		t.Fatalf("bad device key: %d", code)
	}
	if code := call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: "et_bogus"}, nil); code != 403 {
		t.Fatalf("bad enrollment token: %d", code)
	}
}

func TestStagedRolloutAcrossFleet(t *testing.T) {
	s, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)

	const n = 200
	keys := make([]string, n)
	for i := range keys {
		var er api.EnrollResponse
		if code := call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Info: api.DeviceInfo{Hostname: fmt.Sprintf("h%d", i), OS: "linux", Arch: "amd64", AgentVersion: "1.0"}}, &er); code != 200 {
			t.Fatalf("enroll: %d", code)
		}
		keys[i] = er.DeviceKey
	}
	heartbeatAll := func(version string) (offered int, offer *api.UpdateInstruction) {
		for _, k := range keys {
			var hr api.HeartbeatResponse
			call(t, ts, k, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{
				Info: api.DeviceInfo{OS: "linux", Arch: "amd64", AgentVersion: "1.0"}, Packages: map[string]string{"app": version},
			}, &hr)
			if hr.Update != nil {
				offered++
				offer = hr.Update
			}
		}
		return
	}

	rel := upload(t, ts, "app", "2.0.0")
	other := upload(t, ts, "app", "1.9.0")
	var dep api.Deployment
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID, Strategy: &api.Strategy{
		Stages: []int{10, 100}, StageDurationSeconds: 3600, AutoAdvance: true, MaxFailureRate: 0.5, MinSamples: 5,
	}}, &dep)

	offered, offer := heartbeatAll("1.0.0")
	if offered < 8 || offered > 40 {
		t.Fatalf("10%% stage offered to %d of %d devices", offered, n)
	}
	if offer.Version != "2.0.0" || offer.Signature == "" || offer.SHA256 != rel.SHA256 {
		t.Fatalf("bad instruction: %+v", offer)
	}

	// An offered device may download its artifact, but not other releases.
	var key string
	for _, k := range keys {
		var hr api.HeartbeatResponse
		call(t, ts, k, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Packages: map[string]string{"app": "1.0.0"}}, &hr)
		if hr.Update != nil {
			key = k
			break
		}
	}
	if code := call(t, ts, key, "GET", "/api/v1/agent/releases/"+rel.ID+"/artifact", nil, nil); code != 200 {
		t.Fatalf("download offered artifact: %d", code)
	}
	if code := call(t, ts, key, "GET", "/api/v1/agent/releases/"+other.ID+"/artifact", nil, nil); code != 403 {
		t.Fatalf("download non-offered artifact: %d", code)
	}

	// Soak time elapses: the engine advances to 100%.
	s.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(t, ts, admin, "GET", "/api/v1/deployments/"+dep.ID, nil, &struct{ Deployment *api.Deployment }{&dep})
	if dep.CurrentPercent() != 100 {
		t.Fatalf("expected advance to 100%%, at %d%%", dep.CurrentPercent())
	}
	if offered, _ := heartbeatAll("1.0.0"); offered != n {
		t.Fatalf("100%% stage offered to %d of %d", offered, n)
	}
	// Devices reporting the new version are confirmed and offered nothing.
	if offered, _ := heartbeatAll("2.0.0"); offered != 0 {
		t.Fatalf("up-to-date devices offered updates: %d", offered)
	}
	call(t, ts, admin, "GET", "/api/v1/deployments/"+dep.ID, nil, &struct{ Deployment *api.Deployment }{&dep})
	if dep.Stats.Succeeded != n {
		t.Fatalf("expected %d confirmed successes, got %+v", n, dep.Stats)
	}

	// Pausing stops new offers.
	var rel3 api.Release = upload(t, ts, "app", "3.0.0")
	var dep3 api.Deployment
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel3.ID, Strategy: &api.Strategy{Stages: []int{100}}}, &dep3)
	call(t, ts, admin, "POST", "/api/v1/deployments/"+dep3.ID+"/pause", nil, nil)
	if offered, _ := heartbeatAll("2.0.0"); offered != 0 {
		t.Fatalf("paused deployment offered to %d devices", offered)
	}
	call(t, ts, admin, "POST", "/api/v1/deployments/"+dep3.ID+"/resume", nil, nil)
	if offered, _ := heartbeatAll("2.0.0"); offered != n {
		t.Fatalf("resumed deployment offered to %d devices", offered)
	}
	// Completing retires the rollout.
	if code := call(t, ts, admin, "POST", "/api/v1/deployments/"+dep3.ID+"/complete", nil, nil); code != 200 {
		t.Fatalf("complete: %d", code)
	}
	if offered, _ := heartbeatAll("2.0.0"); offered != 0 {
		t.Fatalf("completed deployment offered to %d devices", offered)
	}
	if code := call(t, ts, admin, "POST", "/api/v1/deployments/"+dep3.ID+"/resume", nil, nil); code != 409 {
		t.Fatalf("resuming a completed deployment: %d", code)
	}
}

func TestOfflineDetection(t *testing.T) {
	s, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token}, &er)
	var sum api.FleetSummary
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.Online != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	s.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	call(t, ts, admin, "GET", "/api/v1/summary", nil, &sum)
	if sum.Offline != 1 {
		t.Fatalf("summary after an hour: %+v", sum)
	}
}
