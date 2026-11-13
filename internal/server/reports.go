package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"math"
	"mime"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Customer reports summarise a client's devices over a period: how
// available they were, what alerted, what was updated, what was replaced
// and what maintenance happened. They are built from data Armada already
// keeps (history, alerts, update progress, the activity log), on demand or
// emailed every week or month.

const (
	availabilityTarget = 99.0
	maxReportRange     = 92 * 24 * time.Hour
	maxReportAlerts    = 200
	reportRetryAfter   = time.Hour
	// alertRetention keeps resolved alerts long enough for quarterly reports.
	alertRetention = 100 * 24 * time.Hour
)

// reportPeriod resolves a named period in a time zone. Periods end at
// local midnight; "this-…" periods end now.
func reportPeriod(name string, now time.Time, loc *time.Location) (from, to time.Time, err error) {
	n := now.In(loc)
	today := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	monday := today.AddDate(0, 0, -((int(n.Weekday()) + 6) % 7))
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	switch name {
	case "", "last-month", "monthly":
		return month.AddDate(0, -1, 0), month, nil
	case "this-month":
		return month, now, nil
	case "last-week", "weekly":
		return monday.AddDate(0, 0, -7), monday, nil
	case "this-week":
		return monday, now, nil
	case "last-7d":
		return today.AddDate(0, 0, -7), today, nil
	case "last-30d":
		return today.AddDate(0, 0, -30), today, nil
	}
	return from, to, fmt.Errorf("unknown period %q (use last-month, this-month, last-week, this-week, last-7d or last-30d, or from and to)", name)
}

// reportRequest is what a report is about.
type reportRequest struct {
	scope    api.Selector
	from, to time.Time
	loc      *time.Location
}

func parseReportRequest(r *http.Request, now time.Time) (reportRequest, error) {
	q := r.URL.Query()
	rr := reportRequest{loc: time.UTC, scope: api.Selector{Channel: q.Get("channel")}}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return rr, fmt.Errorf("label must look like key=value")
		}
		if rr.scope.Labels == nil {
			rr.scope.Labels = map[string]string{}
		}
		rr.scope.Labels[k] = v
	}
	if tz := q.Get("tz"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return rr, fmt.Errorf("unknown time zone %q", tz)
		}
		rr.loc = loc
	}
	var err error
	if f, t := q.Get("from"), q.Get("to"); f != "" || t != "" {
		if rr.from, err = time.ParseInLocation("2006-01-02", f, rr.loc); err != nil {
			return rr, fmt.Errorf("from must be a date (2026-10-01)")
		}
		if rr.to, err = time.ParseInLocation("2006-01-02", t, rr.loc); err != nil {
			return rr, fmt.Errorf("to must be a date (2026-10-31)")
		}
		rr.to = rr.to.AddDate(0, 0, 1) // inclusive
	} else if rr.from, rr.to, err = reportPeriod(q.Get("period"), now, rr.loc); err != nil {
		return rr, err
	}
	if !rr.to.After(rr.from) {
		return rr, errors.New("the period must end after it starts")
	}
	if rr.to.Sub(rr.from) > maxReportRange {
		return rr, errors.New("reports cover at most 92 days")
	}
	return rr, nil
}

// reportTitle names what a report covers.
func reportTitle(p *principal, scope api.Selector) string {
	if t := scopeText(scope); t != "" {
		return t
	}
	if !p.scoped() {
		return "Whole fleet"
	}
	if sc := p.scopes(); len(sc) == 1 {
		return scopeText(sc[0])
	}
	return "All your devices"
}

func scopeText(sel api.Selector) string {
	var parts []string
	if sel.Channel != "" {
		parts = append(parts, "channel "+sel.Channel)
	}
	keys := make([]string, 0, len(sel.Labels))
	for k := range sel.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+sel.Labels[k])
	}
	return strings.Join(parts, " ")
}

// buildReport assembles a report for the devices p can see that match scope.
func (s *Server) buildReport(ctx context.Context, p *principal, rr reportRequest) (*api.Report, error) {
	now := s.now()
	rep := &api.Report{Title: reportTitle(p, rr.scope), Brand: s.cfg.ReportBrand, Scope: rr.scope, From: rr.from, To: rr.to,
		TimeZone: rr.loc.String(), GeneratedAt: now, Devices: []api.ReportDevice{}, Alerts: []api.ReportAlert{}, Certificates: []api.ReportCert{},
		AlertRules: []api.ReportCount{}, Updates: []api.ReportCount{}, Changes: []api.Event{}, Maintenance: []api.ReportMaintWindow{}}
	rep.Summary.AvailabilityGoal = availabilityTarget
	rep.Summary.HealthNow = map[string]int{}
	end := rr.to
	if end.After(now) {
		end = now
	}

	all, err := s.store.ListAllDevices(ctx)
	if err != nil {
		return nil, err
	}
	devs := map[string]*api.Device{}
	var order []*api.Device
	for _, d := range all {
		if !p.sees(d) || !rollout.Matches(rr.scope, d) || !d.EnrolledAt.Before(rr.to) {
			continue
		}
		if d.Lifecycle == api.LifecycleRetired && (d.LifecycleInfo == nil || d.LifecycleInfo.ChangedAt.Before(rr.from)) {
			continue // retired before the period
		}
		s.decorate(d)
		devs[d.ID] = d
		order = append(order, d)
	}

	// Maintenance that holds alerts is planned downtime: it's listed, and
	// left out of availability.
	maint, err := s.store.ListMaintenance(ctx)
	if err != nil {
		return nil, err
	}

	// Availability from the "online" history series.
	res := s.cfg.HistoryResolution
	results := map[string]availResult{}
	histFrom := rr.from
	if res > 0 {
		perHour := max(1, int(time.Hour/res))
		if oldest := now.Add(-s.cfg.HistoryRetention); histFrom.Before(oldest) {
			histFrom = oldest
			rep.Notes = append(rep.Notes, fmt.Sprintf("History is kept for %d days, so availability covers %s onwards.",
				int(s.cfg.HistoryRetention.Hours()/24), oldest.In(rr.loc).Format("2 Jan 2006")))
		}
		// When each maintenance window was in force during the period.
		planned := map[string][]rollout.Interval{}
		for _, m := range maint {
			if !m.Holds() {
				continue
			}
			from, to := maxTime(histFrom, m.StartsAt), end
			if m.EndsAt != nil && m.EndsAt.Before(to) {
				to = *m.EndsAt
			}
			if m.Recurring() {
				planned[m.ID] = rollout.WindowIntervals(api.Strategy{Windows: m.Windows, Days: m.Days, TimeZone: m.TimeZone}, from, to)
			} else if to.After(from) {
				planned[m.ID] = []rollout.Interval{{Start: from, End: to}}
			}
		}
		measure := func(d *api.Device, counts map[int64]int) {
			var ivs []rollout.Interval
			for _, m := range maint {
				if len(planned[m.ID]) > 0 && maintenanceCovers(m, d) {
					ivs = append(ivs, planned[m.ID]...)
				}
			}
			results[d.ID] = measureAvailability(counts, maxTime(histFrom, d.EnrolledAt), end, perHour, rollout.MergeIntervals(ivs))
		}
		if end.After(histFrom) {
			cur, counts := "", map[int64]int{}
			flush := func() {
				if d := devs[cur]; d != nil {
					measure(d, counts)
				}
			}
			err := s.store.OnlineHours(ctx, histFrom, end, func(id string, hour time.Time, n int) {
				if id != cur {
					flush()
					cur, counts = id, map[int64]int{}
				}
				counts[hour.Unix()] += n
			})
			if err != nil {
				return nil, err
			}
			flush()
			for _, d := range order {
				if _, done := results[d.ID]; !done {
					measure(d, nil) // never checked in during the period
				}
			}
		}
		rep.Notes = append(rep.Notes, "Availability is the share of time each device was checking in with the server, measured per hour, "+
			"leaving out planned maintenance (maintenance windows that hold alerts). Downtime during maintenance isn't counted against it.")
	} else {
		rep.Notes = append(rep.Notes, "Availability isn't shown: history is turned off on this server.")
	}

	lines := map[string]*api.ReportDevice{}
	var availSum float64
	var availN int
	for _, d := range order {
		platform := d.Info.OS + "/" + d.Info.Arch
		if d.ParentID != "" {
			platform = "ble/" + d.Info.Model
		}
		line := api.ReportDevice{ID: d.ID, Name: d.Name, Lifecycle: d.Lifecycle, Platform: platform, Health: d.Health,
			Packages: d.Packages, LastSeenAt: d.LastSeenAt}
		if r, ok := results[d.ID]; ok {
			if r.maintenance > 0 {
				m := int(math.Round(r.maintenance))
				line.MaintenanceMinutes = &m
			}
			if r.measured {
				a := math.Round(r.availability*100) / 100
				down := int(math.Round(r.downtime))
				line.Availability, line.DowntimeMinutes = &a, &down
				if d.Lifecycle == api.LifecycleActive {
					availSum += a
					availN++
					if a < availabilityTarget {
						rep.Summary.BelowTarget++
					}
				}
			}
		}
		switch d.Lifecycle {
		case api.LifecycleSpare:
			rep.Summary.Spare++
		case api.LifecycleRepair:
			rep.Summary.InRepair++
		case api.LifecycleRetired:
			rep.Summary.Retired++
		default:
			rep.Summary.Active++
		}
		if d.Lifecycle != api.LifecycleRetired {
			rep.Summary.HealthNow[d.Health]++
		}
		rep.Devices = append(rep.Devices, line)
	}
	rep.Summary.Devices = len(order)
	if availN > 0 {
		v := math.Round(availSum/float64(availN)*100) / 100
		rep.Summary.Availability = &v
	}
	for i := range rep.Devices {
		lines[rep.Devices[i].ID] = &rep.Devices[i]
	}

	// Alerts about these devices (device-level conditions; rollout alerts
	// summarise across clients and are covered by the updates section).
	alerts, err := s.store.ListAlertsBetween(ctx, rr.from, rr.to)
	if err != nil {
		return nil, err
	}
	byRule := map[string]int{}
	var ttrSum float64
	var ttrN int
	for _, a := range alerts {
		line := lines[a.DeviceID]
		if line == nil || a.DeploymentID != "" {
			continue
		}
		line.Alerts++
		byRule[a.RuleName]++
		if a.Severity == api.HealthCritical {
			rep.Summary.AlertsCritical++
		} else {
			rep.Summary.AlertsWarning++
		}
		ra := api.ReportAlert{Rule: a.RuleName, Severity: a.Severity, DeviceID: a.DeviceID, DeviceName: line.Name, Summary: a.Summary,
			StartedAt: a.StartedAt, ResolvedAt: a.ResolvedAt}
		ra.ClosedOutOfService = a.ClosedOutOfService
		if a.ResolvedAt != nil {
			m := int(math.Round(a.ResolvedAt.Sub(a.StartedAt).Minutes()))
			ra.DurationMinutes = &m
			if !a.ClosedOutOfService { // closing because the device left service isn't a fix
				ttrSum += float64(m)
				ttrN++
			}
		}
		if len(rep.Alerts) < maxReportAlerts {
			rep.Alerts = append(rep.Alerts, ra)
		}
	}
	if ttrN > 0 {
		v := math.Round(ttrSum/float64(ttrN)*10) / 10
		rep.Summary.MeanTimeToResolveMinutes = &v
	}
	if n := rep.Summary.AlertsCritical + rep.Summary.AlertsWarning; n > maxReportAlerts {
		rep.Notes = append(rep.Notes, fmt.Sprintf("The alert list shows the first %d of %d alerts.", maxReportAlerts, n))
	}
	rep.AlertRules = sortedCounts(byRule, nil)

	// Updates finished in the period.
	ups, err := s.store.DeviceUpdatesBetween(ctx, rr.from, rr.to)
	if err != nil {
		return nil, err
	}
	okBy, failBy := map[string]int{}, map[string]int{}
	for _, u := range ups {
		line := lines[u.DeviceID]
		if line == nil {
			continue
		}
		key := u.Package + " " + u.Version
		switch u.Status {
		case api.UpdateSucceeded:
			line.UpdatesSucceeded++
			rep.Summary.UpdatesSucceeded++
			okBy[key]++
		case api.UpdateFailed, api.UpdateRolledBack:
			line.UpdatesFailed++
			rep.Summary.UpdatesFailed++
			failBy[key]++
		}
	}
	for k := range failBy {
		if _, ok := okBy[k]; !ok {
			okBy[k] = 0
		}
	}
	rep.Updates = sortedCounts(okBy, failBy)

	// Replacements and lifecycle changes.
	evs, err := s.store.ListEventsBetween(ctx, rr.from, rr.to, []string{"device.lifecycle", "device.replaced", "device.replacement"}, 1000)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		if lines[e.DeviceID] == nil {
			continue
		}
		rep.Changes = append(rep.Changes, *e)
		if e.Type == "device.replacement" { // the new device's side of a swap
			rep.Summary.Replacements++
		}
	}
	if oldest := now.Add(-s.cfg.EventRetention); s.cfg.EventRetention > 0 && rr.from.Before(oldest) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("The activity log is kept for %d days, so device changes before %s aren't listed.",
			int(s.cfg.EventRetention.Hours()/24), oldest.In(rr.loc).Format("2 Jan 2006")))
	}

	// Maintenance that overlapped the period and covered these devices.
	for _, m := range maint {
		if !m.StartsAt.Before(rr.to) || (m.EndsAt != nil && !m.EndsAt.After(rr.from)) {
			continue
		}
		n := 0
		for _, d := range order {
			if maintenanceCovers(m, d) {
				n++
			}
		}
		if n > 0 {
			rep.Maintenance = append(rep.Maintenance, api.ReportMaintWindow{Name: m.Name, Note: m.Note, StartsAt: m.StartsAt, EndsAt: m.EndsAt,
				Windows: strings.TrimSpace(m.Windows + " " + strings.Join(m.Days, ",") + " " + m.TimeZone), Devices: n})
		}
	}
	rep.Summary.MaintenanceWindows = len(rep.Maintenance)

	// Managed certificates on these devices, as they are now.
	dcs, err := s.store.ListDeviceCerts(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, c := range dcs {
		d := devs[c.DeviceID]
		if d == nil || d.Lifecycle == api.LifecycleRetired || c.Installed == nil {
			continue
		}
		rc := api.ReportCert{DeviceID: d.ID, DeviceName: d.Name, Target: c.Target, CommonName: c.Installed.CommonName,
			Issuer: c.Installed.Issuer, NotAfter: c.Installed.NotAfter, DaysLeft: *daysLeft(c.Installed, now)}
		if renewalActive(c.Renewal) {
			rc.Renewal = c.Renewal.Status
		}
		if rc.DaysLeft <= 30 {
			rep.Summary.CertsExpiringSoon++
		}
		rep.Certificates = append(rep.Certificates, rc)
	}
	sort.Slice(rep.Certificates, func(i, j int) bool { return rep.Certificates[i].NotAfter.Before(rep.Certificates[j].NotAfter) })

	// Least available first: that's what a reader looks for.
	sort.SliceStable(rep.Devices, func(i, j int) bool {
		a, b := rep.Devices[i].Availability, rep.Devices[j].Availability
		switch {
		case a != nil && b != nil && *a != *b:
			return *a < *b
		case (a == nil) != (b == nil):
			return a != nil
		}
		return rep.Devices[i].Name < rep.Devices[j].Name
	})
	return rep, nil
}

// availResult is one device's availability over a period.
type availResult struct {
	measured     bool    // false if the device spent the whole period in maintenance
	availability float64 // percent, outside maintenance
	downtime     float64 // minutes, outside maintenance
	maintenance  float64 // minutes in maintenance
}

// measureAvailability works out availability from per-hour check-in counts
// (counts[hour] = samples, perHour of them in a fully online hour) over
// [start, end), leaving out the planned intervals. History only says how
// much of each hour a device was up, not when, so down time in an hour
// with maintenance counts against the maintenance first.
func measureAvailability(counts map[int64]int, start, end time.Time, perHour int, planned []rollout.Interval) availResult {
	var r availResult
	var downOut, expected float64
	next := 0
	for h := start.Truncate(time.Hour); h.Before(end); h = h.Add(time.Hour) {
		ws, we := maxTime(h, start), minTime(h.Add(time.Hour), end)
		window := we.Sub(ws).Minutes()
		if window <= 0 {
			continue
		}
		var inMaint float64
		for next < len(planned) && !planned[next].End.After(ws) {
			next++ // intervals are sorted: skip those that ended already
		}
		for i := next; i < len(planned) && planned[i].Start.Before(we); i++ {
			inMaint += minTime(planned[i].End, we).Sub(maxTime(planned[i].Start, ws)).Minutes()
		}
		inMaint = math.Min(inMaint, window)
		online := math.Min(window, float64(counts[h.Unix()])*60/float64(perHour))
		downOut += math.Max(0, window-online-inMaint)
		expected += window - inMaint
		r.maintenance += inMaint
	}
	if expected >= 1 {
		r.measured = true
		r.availability = math.Max(0, 100*(1-downOut/expected))
		r.downtime = downOut
	}
	return r
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sortedCounts(ok, failed map[string]int) []api.ReportCount {
	out := []api.ReportCount{}
	for k, n := range ok {
		out = append(out, api.ReportCount{Name: k, Count: n, Failed: failed[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count+out[i].Failed != out[j].Count+out[j].Failed {
			return out[i].Count+out[i].Failed > out[j].Count+out[j].Failed
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---- formats ----

func reportCSV(rep *api.Report) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	w.Write([]string{"device_id", "name", "lifecycle", "platform", "availability_percent", "downtime_minutes", "maintenance_minutes", "alerts",
		"updates_succeeded", "updates_failed", "health_now", "last_seen", "packages"})
	for _, d := range rep.Devices {
		avail, down, maint := "", "", "0"
		if d.Availability != nil {
			avail = strconv.FormatFloat(*d.Availability, 'f', 2, 64)
			down = strconv.Itoa(*d.DowntimeMinutes)
		}
		if d.MaintenanceMinutes != nil {
			maint = strconv.Itoa(*d.MaintenanceMinutes)
		}
		pk := make([]string, 0, len(d.Packages))
		for k, v := range d.Packages {
			pk = append(pk, k+"="+v)
		}
		sort.Strings(pk)
		seen := ""
		if !d.LastSeenAt.IsZero() {
			seen = d.LastSeenAt.UTC().Format(time.RFC3339)
		}
		w.Write([]string{d.ID, csvSafe(d.Name), d.Lifecycle, d.Platform, avail, down, maint, strconv.Itoa(d.Alerts),
			strconv.Itoa(d.UpdatesSucceeded), strconv.Itoa(d.UpdatesFailed), d.Health, seen, csvSafe(strings.Join(pk, " "))})
	}
	w.Flush()
	return buf.Bytes()
}

// csvSafe stops spreadsheet apps from treating a value as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Server) reportFilename(rep *api.Report, ext string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, rep.Title)
	loc, _ := time.LoadLocation(rep.TimeZone)
	if loc == nil {
		loc = time.UTC
	}
	return fmt.Sprintf("report-%s-%s.%s", strings.Trim(name, "-"), rep.From.In(loc).Format("2006-01-02"), ext)
}

var reportFuncs = template.FuncMap{
	"date": func(t time.Time, tz string) string {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		return t.In(loc).Format("2 Jan 2006")
	},
	"datetime": func(t time.Time, tz string) string {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		return t.In(loc).Format("2 Jan 15:04")
	},
	"lastday": func(t time.Time, tz string) string {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		return t.In(loc).Add(-time.Second).Format("2 Jan 2006")
	},
	"pct": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return strconv.FormatFloat(*v, 'f', 2, 64) + "%"
	},
	"below": func(v *float64, goal float64) bool { return v != nil && *v < goal },
	"dur": func(m *int) string {
		if m == nil {
			return "open"
		}
		return humanMinutes(*m)
	},
	"mins": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return humanMinutes(int(math.Round(*v)))
	},
	"lifecycle": func(s string) string { return lifecycleLabel[s] },
}

func humanMinutes(m int) string {
	switch {
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 48*60:
		return fmt.Sprintf("%dh %02dm", m/60, m%60)
	}
	return fmt.Sprintf("%.1f days", float64(m)/1440)
}

var reportTemplate = template.Must(template.New("report").Funcs(reportFuncs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Brand}} report: {{.Title}}</title>
<style>
body { font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; color: #16191f; background: #fff; margin: 0; padding: 32px; }
main { max-width: 960px; margin: 0 auto; }
h1 { font-size: 24px; margin: 0 0 4px; } h2 { font-size: 16px; margin: 28px 0 8px; }
.sub, .muted { color: #667085; } .bad { color: #b42318; font-weight: 600; }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; margin-top: 20px; }
.tile { border: 1px solid #e3e6eb; border-radius: 10px; padding: 12px 14px; }
.tile .v { font-size: 22px; font-weight: 600; } .tile .l { color: #667085; font-size: 12px; text-transform: uppercase; letter-spacing: .04em; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid #e3e6eb; vertical-align: top; }
th { color: #667085; font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: .04em; }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
.mono { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 12px; }
footer { margin-top: 32px; color: #667085; font-size: 12px; }
@media print { body { padding: 0; } .tile { break-inside: avoid; } tr { break-inside: avoid; } }
</style></head><body><main>
<div class="sub">{{.Brand}} · device report</div>
<h1>{{.Title}}</h1>
<div class="sub">{{date .From .TimeZone}} – {{lastday .To .TimeZone}} ({{.TimeZone}})</div>
<div class="tiles">
<div class="tile"><div class="l">Devices</div><div class="v">{{.Summary.Devices}}</div><div class="muted">{{.Summary.Active}} in service{{if .Summary.Spare}} · {{.Summary.Spare}} spare{{end}}{{if .Summary.InRepair}} · {{.Summary.InRepair}} in repair{{end}}</div></div>
<div class="tile"><div class="l">Availability</div><div class="v">{{pct .Summary.Availability}}</div><div class="muted">{{if .Summary.BelowTarget}}{{.Summary.BelowTarget}} below {{.Summary.AvailabilityGoal}}%{{else}}target {{.Summary.AvailabilityGoal}}%{{end}}</div></div>
<div class="tile"><div class="l">Alerts</div><div class="v">{{.Summary.AlertsCritical}} / {{.Summary.AlertsWarning}}</div><div class="muted">critical / warning · resolved in {{mins .Summary.MeanTimeToResolveMinutes}} on average</div></div>
<div class="tile"><div class="l">Updates installed</div><div class="v">{{.Summary.UpdatesSucceeded}}</div><div class="muted">{{.Summary.UpdatesFailed}} failed or rolled back</div></div>
<div class="tile"><div class="l">Replacements</div><div class="v">{{.Summary.Replacements}}</div><div class="muted">{{.Summary.MaintenanceWindows}} maintenance windows</div></div>
</div>

<h2>Devices</h2>
{{if .Devices}}<table><thead><tr><th>Device</th><th>State</th><th class="num">Availability</th><th class="num">Downtime</th><th class="num">Maintenance</th><th class="num">Alerts</th><th class="num">Updates</th><th>Health now</th></tr></thead><tbody>
{{range .Devices}}<tr><td>{{.Name}}<div class="muted mono">{{.ID}}</div></td><td>{{lifecycle .Lifecycle}}</td>
<td class="num{{if below .Availability $.Summary.AvailabilityGoal}} bad{{end}}">{{pct .Availability}}</td><td class="num">{{if .DowntimeMinutes}}{{dur .DowntimeMinutes}}{{else}}—{{end}}</td><td class="num">{{if .MaintenanceMinutes}}{{dur .MaintenanceMinutes}}{{else}}—{{end}}</td>
<td class="num">{{.Alerts}}</td><td class="num">{{.UpdatesSucceeded}}{{if .UpdatesFailed}} <span class="bad">({{.UpdatesFailed}} failed)</span>{{end}}</td><td>{{.Health}}</td></tr>
{{end}}</tbody></table>{{else}}<p class="muted">No devices.</p>{{end}}

<h2>Alerts</h2>
{{if .Alerts}}<table><thead><tr><th>Started</th><th>Device</th><th>Alert</th><th>Severity</th><th class="num">Lasted</th></tr></thead><tbody>
{{range .Alerts}}<tr><td>{{datetime .StartedAt $.TimeZone}}</td><td>{{.DeviceName}}</td><td>{{.Rule}}<div class="muted">{{.Summary}}</div></td><td>{{.Severity}}</td><td class="num">{{if .ClosedOutOfService}}closed (out of service){{else}}{{dur .DurationMinutes}}{{end}}</td></tr>
{{end}}</tbody></table>{{else}}<p class="muted">No alerts in this period.</p>{{end}}

{{if .Updates}}<h2>Updates</h2>
<table><thead><tr><th>Release</th><th class="num">Installed</th><th class="num">Failed or rolled back</th></tr></thead><tbody>
{{range .Updates}}<tr><td class="mono">{{.Name}}</td><td class="num">{{.Count}}</td><td class="num">{{.Failed}}</td></tr>
{{end}}</tbody></table>{{end}}

{{if .Changes}}<h2>Replacements and changes</h2>
<table><tbody>{{range .Changes}}<tr><td>{{datetime .Time $.TimeZone}}</td><td>{{.Message}}</td></tr>
{{end}}</tbody></table>{{end}}

{{if .Certificates}}<h2>Certificates</h2>
<table><thead><tr><th>Device</th><th>Service</th><th>Certificate</th><th>Expires</th><th class="num">Days left</th></tr></thead><tbody>
{{range .Certificates}}<tr><td>{{.DeviceName}}</td><td>{{.Target}}</td><td>{{.CommonName}}<div class="muted">{{.Issuer}}</div></td><td>{{date .NotAfter $.TimeZone}}{{if .Renewal}}<div class="muted">renewal in progress</div>{{end}}</td><td class="num{{if lt .DaysLeft 31}} bad{{end}}">{{.DaysLeft}}</td></tr>
{{end}}</tbody></table>{{end}}

{{if .Maintenance}}<h2>Maintenance</h2>
<table><thead><tr><th>Maintenance</th><th>When</th><th class="num">Devices</th></tr></thead><tbody>
{{range .Maintenance}}<tr><td>{{.Name}}{{if .Note}}<div class="muted">{{.Note}}</div>{{end}}</td><td>{{datetime .StartsAt $.TimeZone}}{{if .EndsAt}} – {{datetime .EndsAt $.TimeZone}}{{end}}{{if .Windows}}<div class="muted">{{.Windows}}</div>{{end}}</td><td class="num">{{.Devices}}</td></tr>
{{end}}</tbody></table>{{end}}

<footer>{{range .Notes}}<p>{{.}}</p>{{end}}<p>Generated {{datetime .GeneratedAt $.TimeZone}} ({{.TimeZone}}).</p></footer>
</main></body></html>
`))

func reportHTML(rep *api.Report) ([]byte, error) {
	var buf bytes.Buffer
	if err := reportTemplate.Execute(&buf, rep); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rr, err := parseReportRequest(r, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rep, err := s.buildReport(ctx, principalFrom(ctx), rr)
	if err != nil {
		s.internalError(w, err)
		return
	}
	switch r.URL.Query().Get("format") {
	case "", "json":
		writeJSON(w, http.StatusOK, rep)
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+s.reportFilename(rep, "csv")+`"`)
		w.Write(reportCSV(rep))
	case "html":
		b, err := reportHTML(rep)
		if err != nil {
			s.internalError(w, err)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Write(b)
	default:
		writeError(w, http.StatusBadRequest, "format must be json, csv or html")
	}
}

// ---- scheduled reports ----

// schedulePrincipal is who a scheduled report is generated as: a viewer of
// exactly the schedule's client (or of the whole fleet).
func schedulePrincipal(r *api.ReportSchedule) *principal {
	return &principal{name: "report schedule " + r.Name, grants: []api.Grant{{Role: api.RoleViewer, Scope: r.ClientScope}}}
}

func scheduleLocation(r *api.ReportSchedule) *time.Location {
	if loc, err := time.LoadLocation(r.TimeZone); err == nil && r.TimeZone != "" {
		return loc
	}
	return time.UTC
}

func (s *Server) handleListReportSchedules(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListReportSchedules(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.ReportSchedule{}
	for _, rs := range all {
		if see, _ := alertAccess(p, rs.ClientScope, false); see {
			out = append(out, rs)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateReportSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var rs api.ReportSchedule
	if !readJSON(w, r, &rs) {
		return
	}
	if s.cfg.SMTPRelay.Host == "" {
		writeError(w, http.StatusBadRequest, "scheduled reports are emailed through the server's SMTP relay (-smtp-host), which isn't configured")
		return
	}
	rs.Name = strings.TrimSpace(rs.Name)
	if rs.Period != "weekly" && rs.Period != "monthly" {
		writeError(w, http.StatusBadRequest, "period must be weekly or monthly")
		return
	}
	if rs.TimeZone != "" {
		if _, err := time.LoadLocation(rs.TimeZone); err != nil {
			writeError(w, http.StatusBadRequest, "unknown time zone "+strconv.Quote(rs.TimeZone))
			return
		}
	}
	if len(rs.Recipients) == 0 || len(rs.Recipients) > 20 {
		writeError(w, http.StatusBadRequest, "give 1 to 20 recipients")
		return
	}
	for i, to := range rs.Recipients {
		a, err := mail.ParseAddress(strings.TrimSpace(to))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid recipient "+strconv.Quote(to))
			return
		}
		rs.Recipients[i] = a.Address
	}
	sc, err := pickClientScope(p, rs.ClientScope, false, "reports")
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	rs.ClientScope = sc
	if rs.Name == "" {
		rs.Name = strings.ToUpper(rs.Period[:1]) + rs.Period[1:] + " report: " + reportTitle(schedulePrincipal(&rs), sc)
	}
	rs.ID, rs.CreatedBy, rs.CreatedAt = newID("rps"), p.name, s.now()
	rs.LastPeriod, rs.LastSentAt, rs.LastAttemptAt, rs.LastError = "", nil, nil, ""
	if err := s.store.PutReportSchedule(ctx, &rs); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "report.scheduled", "", "", fmt.Sprintf("%s to %s", rs.Name, strings.Join(rs.Recipients, ", ")))
	writeJSON(w, http.StatusCreated, rs)
}

func (s *Server) loadReportSchedule(w http.ResponseWriter, r *http.Request) (*api.ReportSchedule, bool) {
	rs, err := s.store.GetReportSchedule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "report schedule not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	see, edit := alertAccess(principalFrom(r.Context()), rs.ClientScope, false)
	if !see {
		writeError(w, http.StatusNotFound, "report schedule not found")
		return nil, false
	}
	if !edit {
		writeError(w, http.StatusForbidden, "you can't change this report schedule")
		return nil, false
	}
	return rs, true
}

func (s *Server) handleDeleteReportSchedule(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.loadReportSchedule(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteReportSchedule(r.Context(), rs.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "report.unscheduled", "", "", rs.Name+" deleted")
	w.WriteHeader(http.StatusNoContent)
}

// handleSendReportNow emails the last complete period's report right away.
func (s *Server) handleSendReportNow(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.loadReportSchedule(w, r)
	if !ok {
		return
	}
	now := s.now()
	if rs.LastAttemptAt != nil && now.Sub(*rs.LastAttemptAt) < 5*time.Minute {
		writeError(w, http.StatusTooManyRequests, "this report was just sent; try again in a few minutes")
		return
	}
	from, to, _ := reportPeriod(rs.Period, now, scheduleLocation(rs))
	err := s.sendScheduledReport(r.Context(), rs, from, to, false)
	if err != nil {
		writeError(w, http.StatusBadGateway, "sending failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// reportTick emails scheduled reports whose period has ended. It runs on
// every engine tick; each period is sent once (failures retry hourly).
func (s *Server) reportTick(ctx context.Context, now time.Time) error {
	if s.cfg.SMTPRelay.Host == "" {
		return nil
	}
	all, err := s.store.ListReportSchedules(ctx)
	if err != nil {
		return err
	}
	for _, rs := range all {
		from, to, err := reportPeriod(rs.Period, now, scheduleLocation(rs))
		if err != nil {
			continue
		}
		switch {
		case rs.LastPeriod == from.Format("2006-01-02"):
			continue // sent
		case !to.After(rs.CreatedAt):
			continue // the period ended before the schedule existed
		case now.Before(to.Add(time.Hour)):
			continue // give late check-ins and the hourly roll-up time
		case rs.LastAttemptAt != nil && rs.LastError != "" && now.Sub(*rs.LastAttemptAt) < reportRetryAfter:
			continue
		}
		if err := s.sendScheduledReport(ctx, rs, from, to, true); err != nil {
			s.log.Warn("scheduled report failed", "schedule", rs.Name, "err", err)
		}
	}
	return nil
}

func (s *Server) sendScheduledReport(ctx context.Context, rs *api.ReportSchedule, from, to time.Time, scheduled bool) error {
	now := s.now()
	loc := scheduleLocation(rs)
	rep, err := s.buildReport(ctx, schedulePrincipal(rs), reportRequest{scope: rs.ClientScope, from: from, to: to, loc: loc})
	if err == nil {
		err = s.emailReport(ctx, rs, rep)
	}
	rs.LastAttemptAt = &now
	if err != nil {
		rs.LastError = err.Error()
		s.logEvent(ctx, "report.failed", "", "", fmt.Sprintf("%s could not be sent: %v", rs.Name, err))
	} else {
		rs.LastError, rs.LastSentAt = "", &now
		if scheduled {
			rs.LastPeriod = from.Format("2006-01-02")
		}
		s.logEvent(ctx, "report.sent", "", "", fmt.Sprintf("%s for %s sent to %s", rs.Name, from.In(loc).Format("2 Jan 2006"), strings.Join(rs.Recipients, ", ")))
	}
	if perr := s.store.PutReportSchedule(ctx, rs); perr != nil && err == nil {
		err = perr
	}
	return err
}

// emailReport sends a report as an HTML email with the device table attached as CSV.
func (s *Server) emailReport(ctx context.Context, rs *api.ReportSchedule, rep *api.Report) error {
	html, err := reportHTML(rep)
	if err != nil {
		return err
	}
	relay := s.cfg.SMTPRelay
	relay.To = rs.Recipients
	loc, _ := time.LoadLocation(rep.TimeZone)
	if loc == nil {
		loc = time.UTC
	}
	when := rep.From.In(loc).Format("January 2006")
	if rs.Period == "weekly" {
		when = rep.From.In(loc).Format("2 Jan") + " – " + rep.To.In(loc).Add(-time.Second).Format("2 Jan 2006")
	}
	period := "Monthly"
	if rs.Period == "weekly" {
		period = "Weekly"
	}
	subject := fmt.Sprintf("[%s] %s report: %s, %s", s.cfg.ReportBrand, period, rep.Title, when)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return smtpDeliver(ctx, &relay, mimeReport(relay.From, rs.Recipients, subject, html, s.reportFilename(rep, "csv"), reportCSV(rep)))
}

func mimeReport(from string, to []string, subject string, html []byte, csvName string, csvData []byte) string {
	return mimeMessage(from, to, subject, mimePart{"text/html; charset=utf-8", "", html}, mimePart{"text/csv; charset=utf-8", csvName, csvData})
}

// mimePart is one part of a multipart email; a file name makes it an attachment.
type mimePart struct {
	contentType, fileName string
	data                  []byte
}

// mimeMessage builds a multipart/mixed email, every part base64-encoded.
func mimeMessage(from string, to []string, subject string, parts ...mimePart) string {
	boundary := "armada-" + newID("b")
	var b strings.Builder
	b.WriteString("From: " + from + "\r\nTo: " + strings.Join(to, ", ") + "\r\nSubject: " + mime.QEncoding.Encode("utf-8", subject) +
		"\r\nMIME-Version: 1.0\r\nDate: " + time.Now().Format(time.RFC1123Z) +
		"\r\nContent-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	for _, p := range parts {
		b.WriteString("--" + boundary + "\r\nContent-Type: " + p.contentType + "\r\n")
		if p.fileName != "" {
			b.WriteString("Content-Disposition: attachment; filename=\"" + p.fileName + "\"\r\n")
		}
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString(p.data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}
