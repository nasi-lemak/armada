package server

import (
	"encoding/csv"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// The activity log: filters, paging and CSV export.

const (
	eventPageDefault = 100
	eventPageMax     = 1000
	eventCSVMax      = 50000
)

// parseEventTime accepts RFC 3339 or a date (midnight in loc). A date given
// as an upper bound includes that whole day.
func parseEventTime(v string, loc *time.Location, upper bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	t, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return t, err
	}
	if upper {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// eventFilter reads the activity-log filters from a request.
func (s *Server) eventFilter(r *http.Request, p *principal) (store.EventFilter, error) {
	q := r.URL.Query()
	f := store.EventFilter{
		DeviceID:     q.Get("device_id"),
		DeploymentID: q.Get("deployment_id"),
		Actor:        strings.TrimSpace(q.Get("actor")),
		Text:         strings.TrimSpace(q.Get("q")),
	}
	for _, t := range q["type"] {
		for _, part := range strings.Split(t, ",") {
			if part = strings.TrimSpace(part); part != "" {
				f.Types = append(f.Types, part)
			}
		}
	}
	loc := time.UTC
	if tz := q.Get("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return f, errors.New("unknown time zone " + strconv.Quote(tz))
		}
		loc = l
	}
	var err error
	if v := q.Get("since"); v != "" {
		if f.Since, err = parseEventTime(v, loc, false); err != nil {
			return f, errors.New("since must be a date (2026-10-01) or an RFC 3339 time")
		}
	}
	if v := q.Get("until"); v != "" {
		if f.Until, err = parseEventTime(v, loc, true); err != nil {
			return f, errors.New("until must be a date (2026-10-31) or an RFC 3339 time")
		}
	}
	if v := q.Get("before_id"); v != "" {
		if f.BeforeID, err = strconv.ParseInt(v, 10, 64); err != nil || f.BeforeID <= 0 {
			return f, errors.New("before_id must be an event ID")
		}
	}
	if p.scoped() {
		// Client accounts see events about their own devices only.
		visible, err := s.visibleDeviceIDs(r.Context(), p)
		if err != nil {
			return f, err
		}
		f.DeviceIDs = []string{}
		for id := range visible {
			f.DeviceIDs = append(f.DeviceIDs, id)
		}
	}
	return f, nil
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	f, err := s.eventFilter(r, p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	csvOut := r.URL.Query().Get("format") == "csv"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	switch {
	case csvOut && (limit <= 0 || limit > eventCSVMax):
		limit = eventCSVMax
	case !csvOut && (limit <= 0 || limit > eventPageMax):
		limit = eventPageDefault
	}
	evs, err := s.store.QueryEvents(r.Context(), f, limit)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if evs == nil {
		evs = []*api.Event{}
	}
	if !csvOut {
		writeJSON(w, http.StatusOK, evs)
		return
	}
	names := map[string]string{}
	if devs, err := s.store.ListAllDevices(r.Context()); err == nil {
		for _, d := range devs {
			names[d.ID] = d.Name
		}
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="armada-activity-`+s.now().UTC().Format("2006-01-02")+`.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"id", "time", "type", "actor", "device_id", "device", "deployment_id", "message"})
	for _, e := range evs {
		actor := e.Actor
		if actor == "" {
			actor = "system"
		}
		cw.Write([]string{strconv.FormatInt(e.ID, 10), e.Time.UTC().Format(time.RFC3339), e.Type, csvSafe(actor),
			e.DeviceID, csvSafe(names[e.DeviceID]), e.DeploymentID, csvSafe(e.Message)})
	}
	cw.Flush()
}

// handleEventTypes lists the event types recorded, for filter menus.
func (s *Server) handleEventTypes(w http.ResponseWriter, r *http.Request) {
	types, err := s.store.EventTypes(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	if types == nil {
		types = []string{}
	}
	writeJSON(w, http.StatusOK, types)
}
