package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestMonitorChecks(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"db":"up"}`))
	}))
	defer web.Close()
	ctx := context.Background()
	run := func(c api.CheckSpec) api.CheckResult { return runMonitor(ctx, api.MonitorCheck{Name: "t", Check: c}) }

	if r := run(api.CheckSpec{Type: "http", Target: web.URL, Contains: `"db":"up"`}); r.Status != api.HealthOK {
		t.Fatalf("http ok: %+v", r)
	}
	if r := run(api.CheckSpec{Type: "http", Target: web.URL, Contains: "missing"}); r.Status != api.HealthCritical {
		t.Fatalf("http contains: %+v", r)
	}
	status.Store(503)
	if r := run(api.CheckSpec{Type: "http", Target: web.URL}); r.Status != api.HealthCritical || !strings.Contains(r.Message, "503") {
		t.Fatalf("http 503: %+v", r)
	}
	if r := run(api.CheckSpec{Type: "http", Target: web.URL, ExpectStatus: 503}); r.Status != api.HealthOK {
		t.Fatalf("expected status: %+v", r)
	}

	host := strings.TrimPrefix(web.URL, "http://")
	if r := run(api.CheckSpec{Type: "tcp", Target: host}); r.Status != api.HealthOK {
		t.Fatalf("tcp ok: %+v", r)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := l.Addr().String()
	l.Close()
	if r := run(api.CheckSpec{Type: "tcp", Target: closed, TimeoutSeconds: 2}); r.Status != api.HealthCritical {
		t.Fatalf("tcp closed: %+v", r)
	}

	// Nagios plugin convention.
	for code, want := range map[string]string{"0": api.HealthOK, "1": api.HealthWarning, "2": api.HealthCritical, "3": api.HealthUnknown} {
		r := run(api.CheckSpec{Type: "command", Command: []string{"/bin/sh", "-c", "echo 'QUEUE " + code + " | depth=5'; exit " + code}})
		if r.Status != want || r.Message != "QUEUE "+code {
			t.Errorf("exit %s: got %s %q", code, r.Status, r.Message)
		}
	}
	if r := run(api.CheckSpec{Type: "command", Command: []string{"/bin/sleep", "5"}, TimeoutSeconds: 1}); r.Status != api.HealthUnknown {
		t.Fatalf("command timeout: %+v", r)
	}
}

func TestHealthRunnerScheduling(t *testing.T) {
	h := newHealthRunner(HealthConfig{Disabled: true})
	clock := time.Now()
	h.now = func() time.Time { return clock }
	calls := 0
	h.builtins = []builtinCheck{{"fake", time.Minute, func(context.Context, *HealthConfig) []api.CheckResult {
		calls++
		st := api.HealthOK
		if calls > 1 {
			st = api.HealthCritical
		}
		return []api.CheckResult{{Name: "fake", Status: st}}
	}}}
	h.runDue(context.Background())
	h.runDue(context.Background()) // not due yet
	if calls != 1 {
		t.Fatalf("ran %d times before interval", calls)
	}
	clock = clock.Add(61 * time.Second)
	if !h.runDue(context.Background()) {
		t.Fatal("status change not reported")
	}
	select {
	case <-h.changed:
	default:
		t.Fatal("change signal not sent")
	}
	h.setMonitors([]api.MonitorCheck{{Name: "m", Check: api.CheckSpec{Type: "command", Command: []string{"true"}}}})
	h.runDue(context.Background())
	if snap := h.snapshot(); len(snap) != 2 || snap[1].Name != "monitor m" {
		t.Fatalf("snapshot: %+v", snap)
	}
	h.setMonitors(nil) // a removed monitor's result disappears
	if snap := h.snapshot(); len(snap) != 1 {
		t.Fatalf("stale monitor result kept: %+v", snap)
	}
}

func TestStorageScanWithFakeSmartctl(t *testing.T) {
	dir := t.TempDir()
	fake := dir + "/smartctl"
	script := `#!/bin/sh
case "$*" in
  *--scan-open*) echo '{"devices":[{"name":"/dev/disk0","type":"nvme"},{"name":"/dev/disk4","type":"sat"},{"name":"/dev/disk9","type":"sat"}]}';;
  */dev/disk0) echo '{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"nvme_smart_health_information_log":{"available_spare":100,"available_spare_threshold":10,"percentage_used":85}}';;
  */dev/disk4) echo '{"smartctl":{"exit_status":2}}';;
  */dev/disk9) echo '{"smartctl":{"exit_status":0},"smart_status":{"passed":false}}';;
esac`
	os.WriteFile(fake, []byte(script), 0o755)
	res := checkStorageScan(context.Background(), &HealthConfig{Smartctl: fake, IgnoreDevices: []string{"disk9"}})
	if len(res) != 1 || res[0].Name != "storage /dev/disk0" || res[0].Status != api.HealthWarning {
		t.Fatalf("scan: %+v", res) // disk4 has no SMART (USB bridge), disk9 is ignored
	}
	res = checkStorageScan(context.Background(), &HealthConfig{Smartctl: dir + "/missing"})
	if len(res) != 1 || res[0].Status != api.HealthUnknown {
		t.Fatalf("missing smartctl: %+v", res)
	}
	cfg := &HealthConfig{}
	cfg.defaults()
	if r := diskSpaceResult(cfg, `C:\`, 100, 4); r.Status != api.HealthCritical || *r.Value != 96 {
		t.Fatalf("disk space: %+v", r)
	}
}
