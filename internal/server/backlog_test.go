package server

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestBacklogFillsHistory(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	ctx := t.Context()
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	var er api.EnrollResponse
	call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: "till", Info: api.DeviceInfo{OS: "linux"}}, &er)
	temp := func(v float64) []api.CheckResult {
		return []api.CheckResult{{Name: "cpu temperature", Kind: "temperature", Unit: "°C", Value: &v, Status: api.HealthOK}}
	}
	beat := func(backlog []api.BacklogSample) {
		call(t, ts, er.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
			Metrics: api.Metrics{NumCPU: 4, Load1: 0.5, UptimeSeconds: 999999}, Health: temp(50), Backlog: backlog}, nil)
	}
	beat(nil)
	// Three hours cut off; the hours before now get rolled up meanwhile.
	start := clock
	clock = clock.Add(3 * time.Hour)
	if err := s.store.CompactHistory(ctx, clock, 48*time.Hour, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var backlog []api.BacklogSample
	for i := 1; i < 36; i++ {
		backlog = append(backlog, api.BacklogSample{At: start.Add(time.Duration(i) * 5 * time.Minute), Health: temp(60 + float64(i%5)),
			Metrics: api.Metrics{NumCPU: 4, Load1: 2}})
	}
	backlog = append(backlog, api.BacklogSample{At: clock.Add(time.Hour), Health: temp(99)}) // from the future: dropped
	beat(backlog)

	// Raw samples cover the gap, without "online" (it wasn't reachable).
	series, _, _, err := s.store.DeviceHistory(ctx, er.DeviceID, start, false)
	if err != nil {
		t.Fatal(err)
	}
	pts := map[string]int{}
	for _, sr := range series {
		pts[sr.Name] = len(sr.Points)
	}
	if pts["cpu temperature"] < 36 || pts["load"] < 36 || pts["online"] != 2 {
		t.Fatalf("raw points: %v", pts)
	}
	// The rolled-up hours were redone, so long-range charts show the gap filled.
	series, _, _, _ = s.store.DeviceHistory(ctx, er.DeviceID, start, true)
	for _, sr := range series {
		if sr.Name != "cpu temperature" {
			continue
		}
		hours := 0
		var max float64
		for _, p := range sr.Points {
			if p.T < clock.Truncate(time.Hour).Unix() {
				hours++
				if p.Max != nil && *p.Max > max {
					max = *p.Max
				}
			}
		}
		if hours < 3 || max < 64 || max >= 99 {
			t.Fatalf("hourly: %d hours, max %v: %+v", hours, max, sr.Points)
		}
	}
	// The outage was recorded too (network: it stayed up).
	outs, _ := s.store.ListOutages(ctx, start, clock, er.DeviceID)
	if len(outs) != 1 || outs[0].Cause != api.OutageNetwork {
		t.Fatalf("outage: %+v", outs)
	}
}
