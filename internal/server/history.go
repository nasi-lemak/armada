package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// historyState throttles sampling: one sample per series per resolution.
type historyState struct {
	mu          sync.Mutex
	last        map[string]time.Time
	lastFleet   time.Time
	lastCompact time.Time
}

// recordHistory samples a device's checks and metrics. Called on every
// heartbeat (and for peripherals on every gateway report); it stores at most
// one sample per series per HistoryResolution.
func (s *Server) recordHistory(ctx context.Context, dev *api.Device, now time.Time) {
	res := s.cfg.HistoryResolution
	if res <= 0 {
		return
	}
	var samples []store.Sample
	h := &s.history
	h.mu.Lock()
	due := func(name string) bool {
		k := dev.ID + "\x00" + name
		if t, ok := h.last[k]; ok && now.Sub(t) < res {
			return false
		}
		h.last[k] = now
		return true
	}
	add := func(name, kind, unit string, v *float64, status string) {
		if due(name) {
			samples = append(samples, store.Sample{DeviceID: dev.ID, Name: name, Kind: kind, Unit: unit, At: now, Value: v, Status: status})
		}
	}
	// "online" has a sample for every period the device checked in; gaps are downtime.
	add("online", "connectivity", "", nil, api.HealthOK)
	kinds := map[string]bool{}
	for _, c := range dev.Checks {
		kinds[c.Kind] = true
		add(c.Name, c.Kind, c.Unit, c.Value, c.Status)
	}
	m := dev.Metrics
	if dev.ParentID != "" {
		if m.RSSI != 0 {
			add("signal", "radio", "dBm", fptr(float64(m.RSSI)), api.HealthOK)
		}
	} else {
		if m.NumCPU > 0 {
			add("load", "cpu", "load avg", fptr(round2(m.Load1)), api.HealthOK)
		}
		// Linux and Android agents report memory and disk checks themselves
		// (possibly a moment after their first heartbeat); for other platforms
		// and older agents, chart the heartbeat's numbers instead.
		reportsOwn := dev.Info.OS == "linux" || dev.Info.OS == "android"
		if reportsOwn {
			kinds["memory"], kinds["disk_space"] = true, true
		}
		if !kinds["memory"] && m.MemTotalBytes > 0 {
			add("memory used", "memory", "%", fptr(round2(100*float64(m.MemTotalBytes-m.MemAvailableBytes)/float64(m.MemTotalBytes))), api.HealthOK)
		}
		if !kinds["disk_space"] && m.DiskTotalBytes > 0 {
			add("disk used", "disk_space", "%", fptr(round2(100*float64(m.DiskTotalBytes-m.DiskFreeBytes)/float64(m.DiskTotalBytes))), api.HealthOK)
		}
	}
	h.mu.Unlock()
	if err := s.store.AddSamples(ctx, samples); err != nil {
		s.log.Error("record history", "device", dev.ID, "err", err)
	}
}

func fptr(v float64) *float64 { return &v }

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// historyTick records a fleet snapshot every resolution and compacts hourly.
func (s *Server) historyTick(ctx context.Context, now time.Time) error {
	res := s.cfg.HistoryResolution
	if res <= 0 {
		return nil
	}
	h := &s.history
	if now.Sub(h.lastFleet) >= res {
		h.lastFleet = now
		devs, err := s.store.ListDevices(ctx)
		if err != nil {
			return err
		}
		var online, offline, warn, crit int
		for _, d := range devs {
			s.decorate(d)
			if d.Status == api.DeviceOnline {
				online++
			} else {
				offline++
			}
			switch d.Health {
			case api.HealthWarning:
				warn++
			case api.HealthCritical:
				crit++
			}
		}
		firing, err := s.store.ListAlerts(ctx, true, 100000)
		if err != nil {
			return err
		}
		if err := s.store.AddFleetSample(ctx, now, online, offline, warn, crit, len(firing)); err != nil {
			return err
		}
	}
	if now.Sub(h.lastCompact) >= 10*time.Minute {
		h.lastCompact = now
		if err := s.store.CompactHistory(ctx, now, s.cfg.HistoryRawRetention, s.cfg.HistoryRetention); err != nil {
			return err
		}
	}
	return nil
}

// parseRange accepts Go durations and day counts ("7d").
func parseRange(v string, def, maxRange time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, errors.New("invalid range")
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d <= 0 {
			return 0, errors.New("invalid range; use e.g. 24h, 7d, 30d")
		}
	}
	return min(d, maxRange), nil
}

func (s *Server) handleDeviceHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	rng, err := parseRange(r.URL.Query().Get("range"), 24*time.Hour, s.cfg.HistoryRetention)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	from := now.Add(-rng)
	hourly := rng > s.cfg.HistoryRawRetention
	series, oks, ns, err := s.store.DeviceHistory(ctx, d.ID, from, hourly)
	if err != nil {
		s.internalError(w, err)
		return
	}
	resolution := int64(s.cfg.HistoryResolution.Seconds())
	if hourly {
		resolution = 3600
	}
	// Availability of "online" is samples received over samples expected since
	// the first sample in the range (so a newly enrolled device, or history
	// enabled recently, isn't counted as down before it existed).
	for i := range series {
		switch {
		case series[i].Name == "online" && len(series[i].Points) > 0:
			start := time.Unix(series[i].Points[0].T, 0)
			if start.Before(from) {
				start = from
			}
			// Sampling slots elapsed, counting the one the first sample is in.
			expected := math.Floor(now.Sub(start).Seconds()/s.cfg.HistoryResolution.Seconds()) + 1
			a := min(float64(ns[i])/expected, 1)
			series[i].Availability = &a
		case series[i].Name != "online" && ns[i] > 0:
			a := float64(oks[i]) / float64(ns[i])
			series[i].Availability = &a
		}
	}
	writeJSON(w, http.StatusOK, api.DeviceHistory{DeviceID: d.ID, From: from, To: now, Resolution: resolution, Series: series})
}

func (s *Server) handleFleetHistory(w http.ResponseWriter, r *http.Request) {
	rng, err := parseRange(r.URL.Query().Get("range"), 24*time.Hour, s.cfg.HistoryRetention)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	// Aim for ~150 points whatever the range.
	bucket := max(int64(s.cfg.HistoryResolution.Seconds()), int64(rng.Seconds()/150))
	bucket = roundBucket(bucket)
	pts, err := s.store.FleetHistory(r.Context(), now.Add(-rng), bucket)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.FleetHistory{From: now.Add(-rng), To: now, Resolution: bucket, Points: pts})
}

// roundBucket snaps a bucket size to a readable step (5m, 15m, 1h, …).
func roundBucket(sec int64) int64 {
	for _, b := range []int64{60, 300, 900, 1800, 3600, 3 * 3600, 6 * 3600, 12 * 3600, 86400} {
		if sec <= b {
			return b
		}
	}
	return 86400
}
