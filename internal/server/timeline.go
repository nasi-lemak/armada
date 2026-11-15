package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// A device's timeline merges its activity log with its alerts (as spans,
// with who acknowledged them), its updates and the maintenance windows
// that cover it. Alert and update events are left out of the log part
// because those records say the same thing in more detail.

// maxTimelineSkip bounds the cursor's skip count: entries sharing one
// millisecond never come close.
const maxTimelineSkip = 10000

var timelineKinds = []string{"alert", "update", "action", "lifecycle", "connectivity", "cert", "health", "config", "maintenance", "other"}

// eventKindTypes are the event types behind each kind ("other" is the rest).
var eventKindTypes = map[string][]string{
	"action":       {"action.*", "bulk.*"},
	"lifecycle":    {"device.enrolled", "device.lifecycle", "device.replaced", "device.replacement", "device.deleted", "device.license", "device.discovered"},
	"connectivity": {"device.online"},
	"cert":         {"cert.*"},
	"health":       {"health.*"},
	"config":       {"config.*"},
	"maintenance":  {"maintenance.*"},
}

// Covered by alert and update records instead.
var timelineSkipEvents = []string{"alert.*", "update.*"}

func eventKind(typ string) string {
	for kind, types := range eventKindTypes {
		for _, t := range types {
			if p, ok := strings.CutSuffix(t, "*"); ok && strings.HasPrefix(typ, p) || t == typ {
				return kind
			}
		}
	}
	return "other"
}

func eventTone(typ string) string {
	switch {
	case strings.Contains(typ, "fail"), typ == "health.critical":
		return "bad"
	case typ == "health.warning", strings.HasSuffix(typ, ".cancelled"):
		return "warn"
	case strings.HasSuffix(typ, ".succeeded"), strings.HasSuffix(typ, ".installed"), typ == "health.ok", typ == "device.online":
		return "good"
	}
	return ""
}

// timelineCursor is where a page ends: the time of its last entry and how
// many entries at exactly that time were already shown.
type timelineCursor struct {
	at   time.Time
	skip int
}

func parseTimelineCursor(v string) (timelineCursor, error) {
	var c timelineCursor
	if v == "" {
		return c, nil
	}
	ms, skip, ok := strings.Cut(v, ".")
	m, err1 := strconv.ParseInt(ms, 10, 64)
	k, err2 := strconv.Atoi(skip)
	if !ok || err1 != nil || err2 != nil || k < 0 || k > maxTimelineSkip {
		return c, errors.New("bad cursor")
	}
	return timelineCursor{at: time.UnixMilli(m), skip: k}, nil
}

func (c timelineCursor) String() string { return fmt.Sprintf("%d.%d", c.at.UnixMilli(), c.skip) }

func (s *Server) handleDeviceTimeline(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	cur, err := parseTimelineCursor(q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	want := map[string]bool{}
	for _, k := range strings.Split(q.Get("kinds"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			if !slices.Contains(timelineKinds, k) {
				writeError(w, http.StatusBadRequest, "kinds must be among "+strings.Join(timelineKinds, ", "))
				return
			}
			want[k] = true
		}
	}
	tl, err := s.deviceTimeline(r, d, want, cur, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tl)
}

// deviceTimeline builds one page. Every source returns its newest
// limit+skip+1 entries at or before the cursor, so the merged page is exact
// and a longer merge means there is more to come.
func (s *Server) deviceTimeline(r *http.Request, d *api.Device, want map[string]bool, cur timelineCursor, limit int) (*api.Timeline, error) {
	ctx := r.Context()
	p := principalFrom(ctx)
	all := len(want) == 0
	n := limit + cur.skip + 1
	var out []api.TimelineEntry
	add := func(e api.TimelineEntry) {
		e.Time = e.Time.Truncate(time.Millisecond)
		if cur.at.IsZero() || !e.Time.After(cur.at) {
			out = append(out, e)
		}
	}

	// Activity log: one query for the kinds with type lists, and one for
	// "other" (everything else), so each returns exactly its newest n.
	base := store.EventFilter{DeviceID: d.ID, ExcludeTypes: timelineSkipEvents, ByTime: true}
	if !cur.at.IsZero() {
		base.Until = cur.at.Add(time.Millisecond)
	}
	var queries []store.EventFilter
	if all {
		queries = append(queries, base)
	} else {
		listed := base
		for k, types := range eventKindTypes {
			if want[k] {
				listed.Types = append(listed.Types, types...)
			}
		}
		if len(listed.Types) > 0 {
			queries = append(queries, listed)
		}
		if want["other"] {
			other := base
			other.ExcludeTypes = slices.Clone(base.ExcludeTypes)
			for _, types := range eventKindTypes {
				other.ExcludeTypes = append(other.ExcludeTypes, types...)
			}
			queries = append(queries, other)
		}
	}
	for _, ef := range queries {
		evs, err := s.store.QueryEvents(ctx, ef, n)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			kind := eventKind(e.Type)
			link := ""
			if e.DeploymentID != "" && !p.scoped() {
				link = "#/deployments/" + e.DeploymentID
			}
			add(api.TimelineEntry{ID: fmt.Sprintf("event:%012d", e.ID), Time: e.Time, Kind: kind, Type: e.Type,
				Title: e.Message, Tone: eventTone(e.Type), Actor: e.Actor, Link: link})
		}
	}

	if all || want["alert"] {
		alerts, err := s.store.ListDeviceAlerts(ctx, d.ID, cur.at, n)
		if err != nil {
			return nil, err
		}
		for _, a := range alerts {
			e := api.TimelineEntry{ID: "alert:" + a.ID, Time: a.StartedAt, End: a.ResolvedAt, Kind: "alert", Type: a.Condition,
				Title: a.RuleName + ": " + a.Summary, Tone: "warn", Ongoing: a.Status == api.AlertFiring, Link: "#/monitoring"}
			if a.Severity == "critical" {
				e.Tone = "bad"
			}
			var det []string
			if a.Count > 1 {
				det = append(det, fmt.Sprintf("fired %d times", a.Count))
			}
			if a.AckedBy != "" {
				ack := "acknowledged by " + a.AckedBy
				if a.AckNote != "" {
					ack += ": " + a.AckNote
				}
				det = append(det, ack)
			}
			if a.ClosedOutOfService {
				det = append(det, "closed when the device left service")
			}
			e.Detail = strings.Join(det, " · ")
			add(e)
		}
	}

	if all || want["update"] {
		ups, err := s.store.ListDeviceUpdates(ctx, "", d.ID)
		if err != nil {
			return nil, err
		}
		for _, u := range ups {
			at, title := u.CreatedAt, "Offered "+u.Package+" "+u.Version
			if api.IsTerminalUpdateStatus(u.Status) {
				at = u.UpdatedAt
				from := ""
				if u.FromVersion != "" {
					from = " from " + u.FromVersion
				}
				switch u.Status {
				case api.UpdateSucceeded:
					title = "Updated " + u.Package + from + " to " + u.Version
				case api.UpdateRolledBack:
					title = "Update of " + u.Package + " to " + u.Version + " rolled back"
				default:
					title = "Update of " + u.Package + from + " to " + u.Version + " failed"
				}
			} else if u.Status != api.UpdatePending {
				title = strings.ToUpper(u.Status[:1]) + u.Status[1:] + " " + u.Package + " " + u.Version
			}
			e := api.TimelineEntry{ID: "update:" + u.DeploymentID, Time: at, Kind: "update", Type: u.Status, Title: title,
				Detail: u.Message, Ongoing: !api.IsTerminalUpdateStatus(u.Status)}
			switch u.Status {
			case api.UpdateSucceeded:
				e.Tone = "good"
			case api.UpdateFailed:
				e.Tone = "bad"
			case api.UpdateRolledBack:
				e.Tone = "warn"
			}
			if !p.scoped() {
				e.Link = "#/deployments/" + u.DeploymentID
			}
			add(e)
		}
	}

	if all || want["maintenance"] {
		ms, err := s.store.ListMaintenance(ctx)
		if err != nil {
			return nil, err
		}
		now := s.now()
		for _, m := range ms {
			if !maintenanceCovers(m, d) {
				continue
			}
			annotateMaintenance(m, now)
			// Windows the caller can't list (the vendor's, seen by a client)
			// show what the device page shows: name and note, not who made it.
			see, _ := maintenanceAccess(p, m)
			det := []string{}
			if m.Windows != "" && see {
				det = append(det, "recurring "+m.Windows+" "+strings.Join(m.Days, ",")+" "+m.TimeZone)
			}
			if m.Note != "" {
				det = append(det, m.Note)
			}
			e := api.TimelineEntry{ID: "maintenance:" + m.ID, Time: m.StartsAt, End: m.EndsAt, Kind: "maintenance", Type: m.State,
				Title: "Maintenance: " + m.Name, Detail: strings.TrimSpace(strings.Join(det, " · ")), Ongoing: m.State == "active"}
			if see {
				e.Actor, e.Link = m.CreatedBy, "#/monitoring"
			}
			add(e)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time)
		}
		return out[i].ID > out[j].ID
	})
	// Drop what the previous page already showed at the cursor's time.
	if !cur.at.IsZero() {
		dropped := 0
		for dropped < cur.skip && dropped < len(out) && out[dropped].Time.Equal(cur.at) {
			dropped++
		}
		out = out[dropped:]
	}
	tl := &api.Timeline{Entries: out}
	if len(out) > limit {
		tl.Entries = out[:limit]
		last := tl.Entries[limit-1].Time
		next := timelineCursor{at: last}
		for _, e := range tl.Entries {
			if e.Time.Equal(last) {
				next.skip++
			}
		}
		if last.Equal(cur.at) {
			next.skip += cur.skip
		}
		tl.Next = next.String()
	}
	if tl.Entries == nil {
		tl.Entries = []api.TimelineEntry{}
	}
	return tl, nil
}
