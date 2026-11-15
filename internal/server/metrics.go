package server

import (
	"bufio"
	"crypto/subtle"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// Prometheus metrics, in the text exposition format, without a client
// library: a few counters and histograms kept as requests are served, and
// gauges read from the store when scraped.

// latencyBuckets are the upper bounds (seconds) of the request histograms.
var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type metrics struct {
	started time.Time

	mu       sync.Mutex
	requests map[requestKey]uint64
	latency  map[string]*histogram // by "METHOD route"

	heartbeats atomic.Uint64
	enrolls    atomic.Uint64
	ticks      atomic.Uint64
	tickErrors atomic.Uint64
	lastTick   atomic.Int64  // unix nanoseconds when the last tick ended
	tickNanos  atomic.Uint64 // how long it took
}

type requestKey struct {
	method, route string
	code          int
}

type histogram struct {
	counts []uint64 // per bucket, not cumulative
	sum    float64
	n      uint64
}

func newMetrics() *metrics {
	return &metrics{started: time.Now(), requests: map[requestKey]uint64{}, latency: map[string]*histogram{}}
}

// routeOf names a request by the pattern it matched, so the number of
// series stays fixed whatever paths clients ask for.
func routeOf(r *http.Request) (method, route string) {
	p := r.Pattern
	if p == "" {
		return r.Method, "unmatched"
	}
	if m, rest, ok := strings.Cut(p, " "); ok {
		return m, rest
	}
	return r.Method, p
}

func (m *metrics) observeHTTP(r *http.Request, code int, d time.Duration) {
	method, route := routeOf(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[requestKey{method, route, code}]++
	if route == "/api/v1/agent/wait" {
		return // long polls: their duration is the hold time, not latency
	}
	k := method + " " + route
	h := m.latency[k]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets)+1)}
		m.latency[k] = h
	}
	sec := d.Seconds()
	i := sort.SearchFloat64s(latencyBuckets, sec)
	h.counts[i]++
	h.sum += sec
	h.n++
}

func (m *metrics) observeTick(d time.Duration, err error) {
	m.ticks.Add(1)
	if err != nil {
		m.tickErrors.Add(1)
	}
	m.tickNanos.Store(uint64(d))
	m.lastTick.Store(time.Now().UnixNano())
}

// ---- exposition ----

type expo struct {
	w    *bufio.Writer
	seen map[string]bool
}

func (e *expo) family(name, typ, help string) {
	if e.seen[name] {
		return
	}
	e.seen[name] = true
	fmt.Fprintf(e.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// sample writes one line; labels are name, value pairs.
func (e *expo) sample(name string, v float64, labels ...string) {
	e.w.WriteString(name)
	if len(labels) > 0 {
		e.w.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				e.w.WriteByte(',')
			}
			e.w.WriteString(labels[i])
			e.w.WriteString(`="`)
			e.w.WriteString(escapeLabel(labels[i+1]))
			e.w.WriteByte('"')
		}
		e.w.WriteByte('}')
	}
	e.w.WriteByte(' ')
	e.w.WriteString(formatFloat(v))
	e.w.WriteByte('\n')
}

func (e *expo) gauge(name, help string, v float64, labels ...string) {
	e.family(name, "gauge", help)
	e.sample(name, v, labels...)
}

func (e *expo) counter(name, help string, v float64, labels ...string) {
	e.family(name, "counter", help)
	e.sample(name, v, labels...)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// handleMetrics serves the metrics. It takes the -metrics-token, or any
// sign-in with fleet-wide viewer access (checked by the route's guard).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	e := &expo{w: bufio.NewWriter(w), seen: map[string]bool{}}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	e.gauge("armada_build_info", "Version of this server.", 1, "version", s.cfg.Version, "go_version", runtime.Version())
	e.gauge("armada_start_time_seconds", "When this server started (Unix time).", float64(s.metrics.started.Unix()))

	// Devices by status and lifecycle, and by health.
	devs, err := s.store.ListAllDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	type devKey struct{ status, lifecycle string }
	byState := map[devKey]int{}
	health := map[string]int{api.HealthOK: 0, api.HealthWarning: 0, api.HealthCritical: 0}
	for _, d := range devs {
		s.decorate(d)
		life := d.Lifecycle
		if life == "" {
			life = api.LifecycleActive
		}
		byState[devKey{d.Status, life}]++
		if life != api.LifecycleRetired {
			if d.Health != "" {
				health[d.Health]++
			}
		}
	}
	for _, st := range []string{api.DeviceOnline, api.DeviceOffline} {
		for _, life := range []string{api.LifecycleActive, api.LifecycleSpare, api.LifecycleRepair, api.LifecycleRetired} {
			e.gauge("armada_devices", "Devices, by connection status and lifecycle.", float64(byState[devKey{st, life}]),
				"status", st, "lifecycle", life)
		}
	}
	for _, h := range sortedKeys(health) {
		e.gauge("armada_devices_health", "Devices that are not retired, by their worst health check.", float64(health[h]), "health", h)
	}

	deps, err := s.store.ListDeployments(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	depStatus := map[string]int{}
	for _, d := range deps {
		depStatus[d.Status]++
	}
	for _, st := range sortedKeys(depStatus) {
		e.gauge("armada_deployments", "Rollouts, by status.", float64(depStatus[st]), "status", st)
	}
	firing, err := s.store.ListAlerts(ctx, true, 100000)
	if err != nil {
		s.internalError(w, err)
		return
	}
	bySeverity := map[string]int{}
	for _, a := range firing {
		sev := a.Severity
		if sev == "" {
			sev = "warning"
		}
		bySeverity[sev]++
	}
	e.family("armada_alerts_firing", "gauge", "Alerts firing now, by severity.")
	for _, sev := range []string{"critical", "warning", "info"} {
		e.sample("armada_alerts_firing", float64(bySeverity[sev]), "severity", sev)
		delete(bySeverity, sev)
	}
	for _, sev := range sortedKeys(bySeverity) {
		e.sample("armada_alerts_firing", float64(bySeverity[sev]), "severity", sev)
	}

	// Agent traffic.
	e.counter("armada_heartbeats_total", "Device check-ins accepted.", float64(s.metrics.heartbeats.Load()))
	e.counter("armada_enrollments_total", "Devices enrolled.", float64(s.metrics.enrolls.Load()))
	e.gauge("armada_push_connections", "Agents holding a long-poll connection open, so actions reach them at once.", float64(s.push.connections()))

	// Queues.
	e.gauge("armada_notification_queue_length", "Alert notifications waiting to be sent.", float64(len(s.notifyQueue)))
	e.gauge("armada_webhook_queue_length", "Event webhook deliveries waiting to be sent.", float64(len(s.hooks.queue)))

	// Engine.
	e.counter("armada_engine_ticks_total", "Rollout and alert engine passes.", float64(s.metrics.ticks.Load()))
	e.counter("armada_engine_tick_errors_total", "Engine passes that failed.", float64(s.metrics.tickErrors.Load()))
	e.gauge("armada_engine_tick_duration_seconds", "How long the last engine pass took.", time.Duration(s.metrics.tickNanos.Load()).Seconds())
	if t := s.metrics.lastTick.Load(); t > 0 {
		e.gauge("armada_engine_last_tick_time_seconds", "When the last engine pass finished (Unix time).", float64(t)/1e9)
	}

	// Storage and backups.
	var dbSize int64
	for _, f := range []string{"armada.db", "armada.db-wal"} {
		if fi, err := os.Stat(filepath.Join(s.cfg.DataDir, f)); err == nil {
			dbSize += fi.Size()
		}
	}
	e.gauge("armada_database_size_bytes", "Size of the database, with its write-ahead log.", float64(dbSize))
	if bs := s.backupStatus(ctx); bs.Enabled {
		e.gauge("armada_backup_enabled", "1 if automatic backups are set up.", 1)
		if bs.LastOKAt != nil {
			e.gauge("armada_backup_last_success_time_seconds", "When the last automatic backup succeeded (Unix time).", float64(bs.LastOKAt.Unix()))
			e.gauge("armada_backup_last_size_bytes", "Size of the last automatic backup.", float64(bs.LastSize))
		}
		failed := 0.0
		if bs.LastError != "" {
			failed = 1
		}
		e.gauge("armada_backup_last_failed", "1 if the last automatic backup failed.", failed)
	} else {
		e.gauge("armada_backup_enabled", "1 if automatic backups are set up.", 0)
	}
	if c := s.cfg.ServingCert; c != nil {
		if leaf := c.Leaf(); leaf != nil {
			e.gauge("armada_tls_cert_expiry_time_seconds", "When the server's TLS certificate expires (Unix time).", float64(leaf.NotAfter.Unix()))
		}
	}

	// Sites reporting to this server.
	if sites, err := s.store.ListSites(ctx); err == nil && len(sites) > 0 {
		e.gauge("armada_sites", "Sites (other Armada servers) reporting here.", float64(len(sites)))
		for _, site := range sites {
			if site.LastReportAt != nil {
				e.gauge("armada_site_last_report_time_seconds", "When each site last reported (Unix time).", float64(site.LastReportAt.Unix()), "site", site.Name)
			}
		}
	}

	// HTTP.
	s.metrics.mu.Lock()
	keys := make([]requestKey, 0, len(s.metrics.requests))
	for k := range s.metrics.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.code < b.code
	})
	e.family("armada_http_requests_total", "counter", "HTTP requests, by route pattern and status code.")
	for _, k := range keys {
		e.sample("armada_http_requests_total", float64(s.metrics.requests[k]), "method", k.method, "route", k.route, "code", strconv.Itoa(k.code))
	}
	e.family("armada_http_request_duration_seconds", "histogram", "Time to answer HTTP requests, by route pattern (agent long polls left out).")
	for _, k := range sortedKeys(s.metrics.latency) {
		h := s.metrics.latency[k]
		method, route, _ := strings.Cut(k, " ")
		var cum uint64
		for i, le := range latencyBuckets {
			cum += h.counts[i]
			e.sample("armada_http_request_duration_seconds_bucket", float64(cum), "method", method, "route", route, "le", formatFloat(le))
		}
		e.sample("armada_http_request_duration_seconds_bucket", float64(h.n), "method", method, "route", route, "le", "+Inf")
		e.sample("armada_http_request_duration_seconds_sum", h.sum, "method", method, "route", route)
		e.sample("armada_http_request_duration_seconds_count", float64(h.n), "method", method, "route", route)
	}
	s.metrics.mu.Unlock()

	// Go runtime.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	e.gauge("go_goroutines", "Goroutines that currently exist.", float64(runtime.NumGoroutine()))
	e.gauge("go_memstats_heap_alloc_bytes", "Heap bytes allocated and in use.", float64(ms.HeapAlloc))
	e.gauge("go_memstats_sys_bytes", "Bytes obtained from the system.", float64(ms.Sys))
	e.counter("go_gc_cycles_total", "Completed garbage collections.", float64(ms.NumGC))
	e.w.Flush()
}

// metricsAuth lets the -metrics-token through to the metrics; anything else
// goes through the usual sign-in check.
func (s *Server) metricsAuth(guarded http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := s.cfg.MetricsToken; t != "" && subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(t)) == 1 {
			s.handleMetrics(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
