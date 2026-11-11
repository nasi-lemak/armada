package server

import (
	"context"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestScheduledAndWindowedRollout(t *testing.T) {
	s, ts := setup(t)
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) // Monday noon UTC
	clock := base
	s.now = func() time.Time { return clock }
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Info: api.DeviceInfo{OS: "linux", Arch: "amd64"}}, &er)
	offered := func() bool {
		var hr api.HeartbeatResponse
		call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Arch: "amd64"}, Packages: map[string]string{"app": "1.0.0"}}, &hr)
		return hr.Update != nil
	}
	rel := upload(t, ts, "app", "2.0.0")
	start := base.Add(6 * time.Hour) // Monday 18:00 UTC
	var dep api.Deployment
	code := call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID, Strategy: &api.Strategy{
		Stages: []int{100}, StartAt: &start, Windows: "20:00-23:00", TimeZone: "UTC",
	}}, &dep)
	if code != 201 || dep.Status != api.DeploymentScheduled {
		t.Fatalf("create: %d %s", code, dep.Status)
	}
	if offered() {
		t.Fatal("offered before the start time")
	}
	if code := call(t, ts, admin, "POST", "/api/v1/deployments/"+dep.ID+"/pause", nil, nil); code != 409 {
		t.Fatalf("pause scheduled: %d", code)
	}

	clock = start.Add(time.Minute) // started, but 18:01 is outside 20:00-23:00
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	call(t, ts, admin, "GET", "/api/v1/deployments/"+dep.ID, nil, &struct{ Deployment *api.Deployment }{&dep})
	if dep.Status != api.DeploymentRunning {
		t.Fatalf("not started: %s", dep.Status)
	}
	if offered() {
		t.Fatal("offered outside the window")
	}
	clock = base.Add(9 * time.Hour) // 21:00
	if !offered() {
		t.Fatal("not offered inside the window")
	}
}
