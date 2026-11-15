package server

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// TestScale times the main endpoints with many devices. It's slow, so it
// only runs when asked: ARMADA_SCALE_DEVICES=5000 go test -run TestScale ./internal/server
func TestScale(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("ARMADA_SCALE_DEVICES"))
	if n == 0 {
		t.Skip("set ARMADA_SCALE_DEVICES to run")
	}
	s, ts := setup(t)
	ctx := t.Context()
	customers := []string{"acme", "globex", "initech", "umbrella", "hooli"}
	toks := map[string]string{}
	for _, c := range customers {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c, "site": c + "-hq"}}, &tok)
		toks[c] = tok.Token
	}
	upload(t, ts, "kiosk", "1.0.0")
	upload(t, ts, "kiosk", "2.0.0")
	call(t, ts, admin, "POST", "/api/v1/channels", api.NotificationChannel{Name: "ops", Type: "webhook", Webhook: &api.WebhookConfig{URL: "http://127.0.0.1:1/x"}}, nil)

	type dev struct{ id, key string }
	devs := make([]dev, n)
	start := time.Now()
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				c := customers[i%len(customers)]
				var er api.EnrollResponse
				call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: toks[c], Name: fmt.Sprintf("dev-%05d", i),
					Info: api.DeviceInfo{OS: "linux", Arch: "arm64", Serial: fmt.Sprintf("SN%06d", i)}}, &er)
				devs[i] = dev{er.DeviceID, er.DeviceKey}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	enrollTime := time.Since(start)

	v := func(f float64) *float64 { return &f }
	beat := func(i int, conn *api.ConnectivityReport) time.Duration {
		ver := "2.0.0"
		if i%3 == 0 {
			ver = "1.0.0"
		}
		st := api.HealthOK
		if i%50 == 0 {
			st = api.HealthWarning
		}
		req := api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux", Arch: "arm64", Serial: fmt.Sprintf("SN%06d", i)},
			Metrics: api.Metrics{NumCPU: 4, Load1: 0.5, UptimeSeconds: 100000, MemTotalBytes: 4 << 30, MemAvailableBytes: 2 << 30},
			Packages: map[string]string{"kiosk": ver}, Connectivity: conn,
			Health: []api.CheckResult{
				{Name: "disk /", Kind: "disk_space", Status: st, Value: v(40), Unit: "%"},
				{Name: "memory", Kind: "memory", Status: api.HealthOK, Value: v(50), Unit: "%"},
				{Name: "cpu temperature", Kind: "temperature", Status: api.HealthOK, Value: v(55), Unit: "°C"},
				{Name: "kiosk service", Kind: "service", Status: api.HealthOK},
			},
			Certs: []api.CertReport{{Target: "mosquitto", Info: &api.CertInfo{Subject: "CN=x", NotBefore: time.Now().Add(-300 * 24 * time.Hour),
				NotAfter: time.Now().Add(time.Duration(10+i%200) * 24 * time.Hour)}}}}
		t0 := time.Now()
		call(t, ts, devs[i].key, "POST", "/api/v1/agent/heartbeat", req, nil)
		return time.Since(t0)
	}
	var lat []time.Duration
	var mu sync.Mutex
	start = time.Now()
	jobs = make(chan int)
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				var conn *api.ConnectivityReport
				if i%7 == 0 {
					since := time.Now().Add(-time.Minute)
					conn = &api.ConnectivityReport{Since: &since, FailedAttempts: 3, LastError: "i/o timeout"}
				}
				d := beat(i, conn)
				mu.Lock()
				lat = append(lat, d)
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	beatTime := time.Since(start)
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	lastSize := 0
	time1 := func(path string, view string) time.Duration {
		best := time.Hour
		for k := 0; k < 3; k++ {
			req, _ := http.NewRequest("GET", ts.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+admin)
			req.Header.Set("Accept-Encoding", "gzip") // count bytes on the wire
			if view != "" {
				req.Header.Set("X-Armada-View", view)
			}
			t0 := time.Now()
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 32<<10)
			size := 0
			for {
				n, err := res.Body.Read(buf)
				size += n
				if err != nil {
					break
				}
			}
			lastSize = size
			res.Body.Close()
			if res.StatusCode != 200 {
				t.Fatalf("%s: %d", path, res.StatusCode)
			}
			best = min(best, time.Since(t0))
		}
		return best
	}
	t.Logf("%d devices: enroll %s (%.0f/s), heartbeats %s (%.0f/s; p50 %s, p99 %s)", n, enrollTime.Round(time.Millisecond), float64(n)/enrollTime.Seconds(),
		beatTime.Round(time.Millisecond), float64(n)/beatTime.Seconds(), lat[len(lat)/2].Round(time.Microsecond), lat[len(lat)*99/100].Round(time.Microsecond))
	for _, c := range []struct{ path, view string }{
		{"/api/v1/summary", ""}, {"/api/v1/devices", ""}, {"/api/v1/devices", "customer=acme"}, {"/api/v1/insights", ""}, {"/api/v1/insights", "customer=acme"},
		{"/api/v1/events?limit=100", ""}, {"/api/v1/events?limit=100", "customer=acme"}, {"/api/v1/search?q=dev-0123", ""},
		{"/api/v1/devices/" + devs[n/2].id, ""}, {"/api/v1/devices/" + devs[n/2].id + "/timeline", ""}, {"/api/v1/certificates", ""},
		{"/api/v1/alerts", ""}, {"/api/v1/groupings", ""}, {"/api/v1/history/fleet", ""}, {"/api/v1/reports?period=last-7d", ""},
		{"/api/v1/site/snapshot", ""},
	} {
		d := time1(c.path, c.view)
		t.Logf("  %-55s %-15s %8s %8.1f KiB on the wire", c.path, c.view, d.Round(100*time.Microsecond), float64(lastSize)/1024)
	}
	if os.Getenv("ARMADA_SCALE_PROFILE") != "" {
		for k := 0; k < 20; k++ {
			time1("/api/v1/summary", "")
			time1("/api/v1/devices/"+devs[n/2].id, "")
		}
	}
	t0 := time.Now()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("  engine tick %s", time.Since(t0).Round(time.Millisecond))
}
