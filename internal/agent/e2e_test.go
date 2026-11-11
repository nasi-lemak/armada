package agent

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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/server"
)

const adminToken = "test-admin-token"

type harness struct {
	t   *testing.T
	srv *server.Server
	ts  *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end test uses /bin/sh")
	}
	cfg := server.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AdminTokens = []string{adminToken}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.DefaultStrategy = api.Strategy{Stages: []int{100}, AutoAdvance: true, MaxFailureRate: 0.2, MinSamples: 1}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return &harness{t: t, srv: srv, ts: ts}
}

func (h *harness) admin(method, path string, in, out any) {
	h.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) upload(pkg, version string, content []byte) *api.Release {
	h.t.Helper()
	return h.uploadOS(pkg, version, "", content)
}

func (h *harness) uploadOS(pkg, version, osName string, content []byte) *api.Release {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("package", pkg)
	mw.WriteField("version", version)
	mw.WriteField("os", osName)
	fw, _ := mw.CreateFormFile("file", "artifact")
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/v1/releases", &buf)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	var res struct{ Release api.Release }
	json.NewDecoder(resp.Body).Decode(&res)
	return &res.Release
}

func (h *harness) deploy(rel *api.Release) *api.Deployment {
	h.t.Helper()
	var d api.Deployment
	h.admin("POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID}, &d)
	return &d
}

func (h *harness) token() string {
	var tok api.EnrollmentToken
	h.admin("POST", "/api/v1/tokens", api.CreateTokenRequest{Channel: "stable", Labels: map[string]string{"site": "test"}}, &tok)
	return tok.Token
}

func (h *harness) deviceUpdates(deviceID string) []*api.DeviceUpdate {
	var det struct{ Updates []*api.DeviceUpdate }
	h.admin("GET", "/api/v1/devices/"+deviceID, nil, &det)
	return det.Updates
}

func newTestAgent(t *testing.T, h *harness, cfg *Config) *Agent {
	t.Helper()
	cfg.ServerURL = h.ts.URL
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func checkIn(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.CheckIn(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Wait()
}

func TestEndToEndPackageUpdateAndRollback(t *testing.T) {
	h := newHarness(t)
	appDir := t.TempDir()
	sh := func(script string) []string { return []string{"/bin/sh", "-c", script} }
	f := false
	cfg := &Config{
		EnrollmentToken: h.token(),
		StateDir:        t.TempDir(),
		AllowExec:       true,
		AllowReboot:     &f,
		Packages: map[string]PackageConfig{
			"app": {
				InstallCommand:  sh(`cp "$ARMADA_ARTIFACT" "` + appDir + `/current" && echo "$ARMADA_VERSION" > "` + appDir + `/version"`),
				HealthCommand:   sh(`grep -q healthy "` + appDir + `/current"`),
				HealthRetries:   2,
				RollbackCommand: sh(`cp "$ARMADA_PREVIOUS_ARTIFACT" "` + appDir + `/current" && echo "$ARMADA_PREVIOUS_VERSION" > "` + appDir + `/version"`),
				VersionCommand:  []string{"cat", appDir + "/version"},
			},
		},
	}
	cfg.Packages["app"] = withFastHealth(cfg.Packages["app"])
	a := newTestAgent(t, h, cfg)
	ctx := context.Background()
	if err := a.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	checkIn(t, a)

	var devs []api.Device
	h.admin("GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 1 || devs[0].Status != api.DeviceOnline || devs[0].Labels["site"] != "test" {
		t.Fatalf("unexpected devices: %+v", devs)
	}
	devID := devs[0].ID

	// 1. A healthy release installs.
	h.deploy(h.upload("app", "1.0.0", []byte("healthy v1")))
	checkIn(t, a) // offered + installed
	checkIn(t, a) // reports new version
	if v := readFile(t, appDir+"/version"); v != "1.0.0" {
		t.Fatalf("installed version = %q", v)
	}
	ups := h.deviceUpdates(devID)
	if len(ups) != 1 || ups[0].Status != api.UpdateSucceeded {
		t.Fatalf("expected success, got %+v", ups[0])
	}

	// 2. A broken release fails its health check and is rolled back.
	bad := h.deploy(h.upload("app", "1.1.0", []byte("broken v1.1")))
	checkIn(t, a)
	if v := readFile(t, appDir+"/version"); v != "1.0.0" {
		t.Fatalf("after rollback version = %q", v)
	}
	if got := readFile(t, appDir+"/current"); got != "healthy v1" {
		t.Fatalf("after rollback artifact = %q", got)
	}
	var detail struct {
		Deployment api.Deployment
		Updates    []*api.DeviceUpdate
	}
	h.admin("GET", "/api/v1/deployments/"+bad.ID, nil, &detail)
	if len(detail.Updates) != 1 || detail.Updates[0].Status != api.UpdateRolledBack {
		t.Fatalf("expected rolled_back, got %+v", detail.Updates)
	}
	// The failure-rate guard pauses the rollout.
	if err := h.srv.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.admin("GET", "/api/v1/deployments/"+bad.ID, nil, &detail)
	if detail.Deployment.Status != api.DeploymentPaused {
		t.Fatalf("expected deployment auto-paused, got %s", detail.Deployment.Status)
	}
	// The failed device is not offered the bad release again.
	checkIn(t, a)
	if v := readFile(t, appDir+"/version"); v != "1.0.0" {
		t.Fatalf("version changed to %q", v)
	}

	// 3. Remote actions.
	var act api.Action
	h.admin("POST", "/api/v1/devices/"+devID+"/actions", api.CreateActionRequest{Type: "exec", Args: []string{"echo", "hello fleet"}}, &act)
	var reboot api.Action
	h.admin("POST", "/api/v1/devices/"+devID+"/actions", api.CreateActionRequest{Type: "reboot"}, &reboot)
	checkIn(t, a)
	var acts []api.Action
	h.admin("GET", "/api/v1/devices/"+devID+"/actions", nil, &acts)
	byID := map[string]api.Action{}
	for _, x := range acts {
		byID[x.ID] = x
	}
	if got := byID[act.ID]; got.Status != api.ActionSucceeded || strings.TrimSpace(got.Output) != "hello fleet" {
		t.Fatalf("exec action: %+v", got)
	}
	if got := byID[reboot.ID]; got.Status != api.ActionRejected {
		t.Fatalf("reboot should be rejected when disallowed: %+v", got)
	}
}

func withFastHealth(p PackageConfig) PackageConfig {
	p.HealthInterval.Duration = 1
	return p
}

func TestUpdateRejectsBadSignature(t *testing.T) {
	h := newHarness(t)
	a := newTestAgent(t, h, &Config{EnrollmentToken: h.token(), StateDir: t.TempDir()})
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := a.fetchArtifact(context.Background(), api.UpdateInstruction{
		Package: "app", Version: "1.0.0", SHA256: strings.Repeat("0", 64), Signature: "AAAA",
	})
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestEnrollmentTokenLimits(t *testing.T) {
	h := newHarness(t)
	var tok api.EnrollmentToken
	h.admin("POST", "/api/v1/tokens", api.CreateTokenRequest{MaxUses: 1}, &tok)
	a1 := newTestAgent(t, h, &Config{EnrollmentToken: tok.Token, StateDir: t.TempDir()})
	if err := a1.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2 := newTestAgent(t, h, &Config{EnrollmentToken: tok.Token, StateDir: t.TempDir()})
	if err := a2.Enroll(context.Background()); err == nil {
		t.Fatal("second enrollment with a single-use token succeeded")
	}
}

func TestSelfUpdate(t *testing.T) {
	h := newHarness(t)
	binDir := t.TempDir()
	exe := filepath.Join(binDir, "armada-agent")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho armada-agent old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	tok := h.token()
	var reexecs []string
	mk := func() *Agent {
		a := newTestAgent(t, h, &Config{EnrollmentToken: tok, StateDir: stateDir})
		a.ExePath = exe
		a.Reexec = func(p string) error { reexecs = append(reexecs, p); return nil }
		return a
	}
	defer func(v string) { Version = v }(Version)
	Version = "1.0.0"

	a := mk()
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	newBin := []byte("#!/bin/sh\necho armada-agent 2.0.0\n")
	dep := h.deploy(h.upload(api.AgentPackage, "2.0.0", newBin))
	checkIn(t, a)
	if len(reexecs) != 1 {
		t.Fatalf("expected a restart into the new binary, got %v", reexecs)
	}
	if got := readFile(t, exe); !strings.Contains(got, "2.0.0") {
		t.Fatalf("binary not replaced: %q", got)
	}
	if got := readFile(t, exe+".prev"); !strings.Contains(got, "old") {
		t.Fatalf("backup missing: %q", got)
	}

	// Simulate the new process starting up.
	Version = "2.0.0"
	a2 := mk()
	if err := a2.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkIn(t, a2)
	var detail struct{ Updates []*api.DeviceUpdate }
	h.admin("GET", "/api/v1/deployments/"+dep.ID, nil, &detail)
	if len(detail.Updates) != 1 || detail.Updates[0].Status != api.UpdateSucceeded {
		t.Fatalf("self-update not confirmed: %+v", detail.Updates)
	}
	if a2.state.get().SelfUpdate != nil {
		t.Fatal("pending self-update not cleared")
	}
}

func TestSelfUpdateCrashLoopRollsBack(t *testing.T) {
	h := newHarness(t)
	binDir := t.TempDir()
	exe := filepath.Join(binDir, "armada-agent")
	os.WriteFile(exe, []byte("new"), 0o755)
	os.WriteFile(exe+".prev", []byte("old"), 0o755)
	stateDir := t.TempDir()
	defer func(v string) { Version = v }(Version)
	Version = "2.0.0"

	var reexecs int
	a := newTestAgent(t, h, &Config{EnrollmentToken: h.token(), StateDir: stateDir})
	a.ExePath = exe
	a.Reexec = func(string) error { reexecs++; return nil }
	a.state.update(func(s *State) {
		s.SelfUpdate = &PendingSelfUpdate{DeploymentID: "dep_x", Version: "2.0.0", FromVersion: "1.0.0", BackupPath: exe + ".prev", Starts: maxSelfUpdateStarts}
	})
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, exe); got != "old" {
		t.Fatalf("previous binary not restored: %q", got)
	}
	st := a.state.get()
	if reexecs != 1 || st.SelfUpdate != nil || len(st.Outbox) != 1 || st.Outbox[0].Status != api.UpdateRolledBack {
		t.Fatalf("unexpected state after rollback: reexecs=%d %+v", reexecs, st)
	}
}

func TestMaintenanceWindow(t *testing.T) {
	ws, err := ParseWindows("22:00-02:00, 12:00-13:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		hhmm string
		want bool
	}{{"23:30", true}, {"01:59", true}, {"02:00", false}, {"12:30", true}, {"15:00", false}} {
		var hh, mm int
		fmt.Sscanf(c.hhmm, "%d:%d", &hh, &mm)
		tm := testTime(hh, mm)
		if got := InWindow(ws, tm); got != c.want {
			t.Errorf("InWindow(%s) = %v, want %v", c.hhmm, got, c.want)
		}
	}
	if _, err := ParseWindows("25:00-01:00"); err == nil {
		t.Error("invalid window accepted")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestFailedFreshInstallStaysFailed(t *testing.T) {
	h := newHarness(t)
	appDir := t.TempDir()
	sh := func(script string) []string { return []string{"/bin/sh", "-c", script} }
	a := newTestAgent(t, h, &Config{
		EnrollmentToken: h.token(),
		StateDir:        t.TempDir(),
		Packages: map[string]PackageConfig{"app": withFastHealth(PackageConfig{
			InstallCommand:  sh(`echo "$ARMADA_VERSION" > "` + appDir + `/version"`),
			HealthCommand:   []string{"false"},
			RollbackCommand: []string{"true"},
			VersionCommand:  []string{"cat", appDir + "/version"},
		})},
	})
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatal(err)
	}
	dep := h.deploy(h.upload("app", "1.0.0", []byte("x")))
	checkIn(t, a) // install fails health; nothing to roll back to
	checkIn(t, a) // device now reports 1.0.0
	var detail struct{ Updates []*api.DeviceUpdate }
	h.admin("GET", "/api/v1/deployments/"+dep.ID, nil, &detail)
	if len(detail.Updates) != 1 || detail.Updates[0].Status != api.UpdateFailed {
		t.Fatalf("failure was overwritten: %+v", detail.Updates[0])
	}
	if !strings.Contains(detail.Updates[0].Message, "no previous version") {
		t.Fatalf("unexpected message: %q", detail.Updates[0].Message)
	}
}

func TestEndToEndHealthMonitoring(t *testing.T) {
	h := newHarness(t)
	var healthy atomic.Bool
	healthy.Store(true)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(500)
		}
	}))
	defer web.Close()
	// Only devices labelled site=test get this monitor (the harness token sets that label).
	var mon api.Monitor
	h.admin("POST", "/api/v1/monitors", api.Monitor{Name: "kiosk-web", Enabled: true,
		Selector: api.Selector{Labels: map[string]string{"site": "test"}},
		Check:    api.CheckSpec{Type: "http", Target: web.URL + "/healthz"}}, &mon)
	h.admin("POST", "/api/v1/monitors", api.Monitor{Name: "elsewhere", Enabled: true,
		Selector: api.Selector{Labels: map[string]string{"site": "other"}},
		Check:    api.CheckSpec{Type: "tcp", Target: "127.0.0.1:1"}}, nil)

	a := newTestAgent(t, h, &Config{EnrollmentToken: h.token(), StateDir: t.TempDir(), Health: HealthConfig{Disabled: true}})
	ctx := context.Background()
	if err := a.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	cycle := func() {
		checkIn(t, a)
		a.health.lastRun = map[string]time.Time{} // make every check due
		a.RunHealthChecks(ctx)
		checkIn(t, a)
	}
	cycle()
	var det struct {
		Device api.Device
		Events []*api.Event
	}
	h.admin("GET", "/api/v1/devices/"+a.DeviceID(), nil, &det)
	if det.Device.Health != api.HealthOK || len(det.Device.Checks) != 1 || det.Device.Checks[0].Name != "monitor kiosk-web" {
		t.Fatalf("expected one passing monitor, got %s %+v", det.Device.Health, det.Device.Checks)
	}

	healthy.Store(false)
	cycle()
	h.admin("GET", "/api/v1/devices/"+a.DeviceID(), nil, &det)
	if det.Device.Health != api.HealthCritical {
		t.Fatalf("expected critical, got %s %+v", det.Device.Health, det.Device.Checks)
	}
	found := false
	for _, e := range det.Events {
		if e.Type == "health.critical" && strings.Contains(e.Message, "monitor kiosk-web ok → critical") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no health transition event: %+v", det.Events)
	}
	var critical []api.Device
	h.admin("GET", "/api/v1/devices?health=critical", nil, &critical)
	if len(critical) != 1 {
		t.Fatalf("health filter: %d devices", len(critical))
	}

	// Disabling the monitor removes it from the device on the next check-in.
	mon.Enabled = false
	h.admin("PUT", "/api/v1/monitors/"+mon.ID, mon, nil)
	cycle()
	det.Device = api.Device{} // decoding doesn't clear fields omitted from the response
	h.admin("GET", "/api/v1/devices/"+a.DeviceID(), nil, &det)
	if len(det.Device.Checks) != 0 || det.Device.Health != api.HealthOK {
		t.Fatalf("disabled monitor still reported: %+v", det.Device.Checks)
	}
}
