package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// HealthConfig tunes the built-in hardware checks.
type HealthConfig struct {
	// Disabled turns off built-in hardware checks (server monitors still run).
	Disabled bool `json:"disabled,omitempty"`
	// DiskWarnPercent / DiskCritPercent flag full filesystems (defaults 85 / 95).
	DiskWarnPercent float64 `json:"disk_warn_percent,omitempty"`
	DiskCritPercent float64 `json:"disk_crit_percent,omitempty"`
	// TempWarnC / TempCritC flag hot sensors in °C (defaults 80 / 90).
	TempWarnC float64 `json:"temperature_warn_c,omitempty"`
	TempCritC float64 `json:"temperature_crit_c,omitempty"`
	// ReadOnlyMountsOK lists mount points that are meant to be read-only
	// (e.g. "/" on a read-only root filesystem), so they are not flagged.
	ReadOnlyMountsOK []string `json:"readonly_mounts_ok,omitempty"`
	// Smartctl is the smartctl binary (default "smartctl" from PATH).
	Smartctl string `json:"smartctl,omitempty"`
	// IgnoreDevices skips block devices by name, e.g. ["sdz"].
	IgnoreDevices []string `json:"ignore_devices,omitempty"`
}

func (h *HealthConfig) defaults() {
	if h.DiskWarnPercent == 0 {
		h.DiskWarnPercent = 85
	}
	if h.DiskCritPercent == 0 {
		h.DiskCritPercent = 95
	}
	if h.TempWarnC == 0 {
		h.TempWarnC = 80
	}
	if h.TempCritC == 0 {
		h.TempCritC = 90
	}
	if h.Smartctl == "" {
		h.Smartctl = "smartctl"
	}
}

// builtinCheck is a hardware check group run on an interval. It returns a
// result per thing it checked (e.g. one per disk).
type builtinCheck struct {
	name     string
	interval time.Duration
	run      func(ctx context.Context, cfg *HealthConfig) []api.CheckResult
}

// healthRunner schedules built-in and server-defined checks and keeps the
// latest result of each.
type healthRunner struct {
	cfg      HealthConfig
	builtins []builtinCheck
	now      func() time.Time

	mu       sync.Mutex
	monitors []api.MonitorCheck
	// allowCommands lets server-defined monitors run commands. A command
	// check is arbitrary code as root, so it needs allow_exec like exec.
	allowCommands bool
	results       map[string][]api.CheckResult // keyed by check group
	lastRun       map[string]time.Time
	changed       chan struct{}
}

func newHealthRunner(cfg HealthConfig) *healthRunner {
	cfg.defaults()
	h := &healthRunner{cfg: cfg, now: time.Now, results: map[string][]api.CheckResult{}, lastRun: map[string]time.Time{}, changed: make(chan struct{}, 1)}
	if !cfg.Disabled {
		h.builtins = platformChecks()
	}
	return h
}

// setMonitors replaces the server-defined checks.
func (h *healthRunner) setMonitors(ms []api.MonitorCheck) {
	h.mu.Lock()
	defer h.mu.Unlock()
	keep := map[string]bool{}
	for _, m := range ms {
		keep["monitor "+m.Name] = true
	}
	for k := range h.results {
		if strings.HasPrefix(k, "monitor ") && !keep[k] {
			delete(h.results, k)
			delete(h.lastRun, k)
		}
	}
	// A changed definition runs again right away.
	for _, m := range ms {
		for _, old := range h.monitors {
			if old.Name == m.Name && fmt.Sprint(old.Check) != fmt.Sprint(m.Check) {
				delete(h.lastRun, "monitor "+m.Name)
			}
		}
	}
	h.monitors = ms
}

// runDue runs every check whose interval has elapsed. It returns true if
// any status changed, so the agent can report promptly.
func (h *healthRunner) runDue(ctx context.Context) bool {
	type job struct {
		key      string
		interval time.Duration
		run      func(context.Context) []api.CheckResult
	}
	var jobs []job
	for _, b := range h.builtins {
		b := b
		jobs = append(jobs, job{b.name, b.interval, func(ctx context.Context) []api.CheckResult { return b.run(ctx, &h.cfg) }})
	}
	h.mu.Lock()
	for _, m := range h.monitors {
		m := m
		iv := time.Duration(m.Check.IntervalSeconds) * time.Second
		if iv < 10*time.Second {
			iv = 60 * time.Second
		}
		jobs = append(jobs, job{"monitor " + m.Name, iv, func(ctx context.Context) []api.CheckResult {
			if m.Check.Type == "command" && !h.allowCommands {
				return []api.CheckResult{{Name: "monitor " + m.Name, Kind: "command", Status: api.HealthUnknown,
					Message: "command checks are disabled on this device (they need allow_exec: true in the agent config)"}}
			}
			return []api.CheckResult{runMonitor(ctx, m)}
		}})
	}
	now := h.now()
	var due []job
	for _, j := range jobs {
		if last, ok := h.lastRun[j.key]; !ok || now.Sub(last) >= j.interval {
			due = append(due, j)
			h.lastRun[j.key] = now
		}
	}
	h.mu.Unlock()

	changed := false
	var wg sync.WaitGroup
	var rmu sync.Mutex
	for _, j := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := j.run(ctx)
			ts := h.now().UTC()
			for i := range res {
				if res[i].CheckedAt.IsZero() {
					res[i].CheckedAt = ts
				}
			}
			rmu.Lock()
			defer rmu.Unlock()
			h.mu.Lock()
			if statusKey(h.results[j.key]) != statusKey(res) {
				changed = true
			}
			h.results[j.key] = res
			h.mu.Unlock()
		}()
	}
	wg.Wait()
	if changed {
		select {
		case h.changed <- struct{}{}:
		default:
		}
	}
	return changed
}

func statusKey(rs []api.CheckResult) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Name + "=" + r.Status
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// snapshot returns all current results, sorted by name.
func (h *healthRunner) snapshot() []api.CheckResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []api.CheckResult{}
	for _, rs := range h.results {
		out = append(out, rs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- server-defined monitors ----

func runMonitor(ctx context.Context, m api.MonitorCheck) api.CheckResult {
	c := m.Check
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r := api.CheckResult{Name: "monitor " + m.Name, Kind: c.Type, Details: map[string]string{}}
	switch c.Type {
	case "systemd":
		checkSystemd(ctx, c, &r)
	case "process":
		checkProcess(c, &r)
	case "http":
		checkHTTP(ctx, c, &r)
	case "tcp":
		checkTCP(ctx, c, &r)
	case "command":
		checkCommand(ctx, c, &r)
	default:
		r.Status, r.Message = api.HealthUnknown, "unsupported check type "+c.Type
	}
	if len(r.Details) == 0 {
		r.Details = nil
	}
	return r
}

func checkSystemd(ctx context.Context, c api.CheckSpec, r *api.CheckResult) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		r.Status, r.Message = api.HealthUnknown, "systemctl is not available on this device"
		return
	}
	out, _, _ := runCommand(ctx, []string{"systemctl", "show", c.Target, "--property=ActiveState,SubState,NRestarts,LoadState"}, nil)
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	r.Details["restarts"] = props["NRestarts"]
	state := props["ActiveState"]
	switch {
	case props["LoadState"] == "not-found":
		r.Status, r.Message = api.HealthCritical, c.Target+" is not installed"
	case state == "active":
		r.Status, r.Message = api.HealthOK, c.Target+" is running"
	case state == "activating" || state == "reloading":
		r.Status, r.Message = api.HealthWarning, c.Target+" is "+state
	case state == "":
		r.Status, r.Message = api.HealthUnknown, "could not query "+c.Target
	default:
		r.Status, r.Message = api.HealthCritical, fmt.Sprintf("%s is %s (%s)", c.Target, state, props["SubState"])
	}
}

func checkHTTP(ctx context.Context, c api.CheckSpec, r *api.CheckResult) {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Target, nil)
	if err != nil {
		r.Status, r.Message = api.HealthUnknown, "bad URL: "+err.Error()
		return
	}
	start := time.Now()
	resp, err := client.Do(req)
	ms := float64(time.Since(start).Milliseconds())
	r.Value, r.Unit = &ms, "ms"
	if err != nil {
		r.Status, r.Message = api.HealthCritical, "request failed: "+shortErr(err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if c.ExpectStatus != 0 {
		ok = resp.StatusCode == c.ExpectStatus
	}
	switch {
	case !ok:
		r.Status, r.Message = api.HealthCritical, fmt.Sprintf("HTTP %d from %s", resp.StatusCode, c.Target)
	case c.Contains != "" && !strings.Contains(string(body), c.Contains):
		r.Status, r.Message = api.HealthCritical, fmt.Sprintf("response does not contain %q", c.Contains)
	case c.WarnMs > 0 && ms > float64(c.WarnMs):
		r.Status, r.Message = api.HealthWarning, fmt.Sprintf("slow: %.0f ms (limit %d ms)", ms, c.WarnMs)
	default:
		r.Status, r.Message = api.HealthOK, fmt.Sprintf("HTTP %d in %.0f ms", resp.StatusCode, ms)
	}
}

func checkTCP(ctx context.Context, c api.CheckSpec, r *api.CheckResult) {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.Target)
	ms := float64(time.Since(start).Milliseconds())
	r.Value, r.Unit = &ms, "ms"
	if err != nil {
		r.Status, r.Message = api.HealthCritical, "cannot connect to "+c.Target+": "+shortErr(err)
		return
	}
	conn.Close()
	if c.WarnMs > 0 && ms > float64(c.WarnMs) {
		r.Status, r.Message = api.HealthWarning, fmt.Sprintf("slow connect: %.0f ms", ms)
		return
	}
	r.Status, r.Message = api.HealthOK, fmt.Sprintf("%s reachable in %.0f ms", c.Target, ms)
}

func checkCommand(ctx context.Context, c api.CheckSpec, r *api.CheckResult) {
	out, code, err := runCommand(ctx, c.Command, nil)
	msg := firstLine(out)
	if i := strings.IndexByte(msg, '|'); i >= 0 { // drop Nagios perfdata
		msg = strings.TrimSpace(msg[:i])
	}
	if err != nil {
		r.Status, r.Message = api.HealthUnknown, describeRun(out, code, err)
		return
	}
	switch code {
	case 0:
		r.Status = api.HealthOK
	case 1:
		r.Status = api.HealthWarning
	case 2:
		r.Status = api.HealthCritical
	default:
		r.Status = api.HealthUnknown
	}
	if c.Contains != "" && !strings.Contains(out, c.Contains) && r.Status == api.HealthOK {
		r.Status, msg = api.HealthCritical, fmt.Sprintf("output does not contain %q", c.Contains)
	}
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", code)
	}
	r.Message = truncateStr(msg, 300)
}

func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && len(s) > 120 {
		s = s[i+2:]
	}
	return truncateStr(s, 200)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func float(v float64) *float64 { return &v }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// diskSpaceResult rates one filesystem's fullness.
func diskSpaceResult(cfg *HealthConfig, mountPoint string, total, avail uint64) api.CheckResult {
	used := 100 * float64(total-avail) / float64(total)
	r := api.CheckResult{Name: "disk-space " + mountPoint, Kind: "disk_space", Value: float(round1(used)), Unit: "%",
		Details: map[string]string{"free_gb": fmt.Sprintf("%.1f", float64(avail)/1e9)}}
	switch {
	case used >= cfg.DiskCritPercent:
		r.Status = api.HealthCritical
	case used >= cfg.DiskWarnPercent:
		r.Status = api.HealthWarning
	default:
		r.Status = api.HealthOK
	}
	r.Message = fmt.Sprintf("%s is %.0f%% full", mountPoint, used)
	return r
}
