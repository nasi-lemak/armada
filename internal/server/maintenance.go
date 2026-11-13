package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Maintenance windows mark devices as expected to be down or noisy. While one
// covers a device:
//   - alerts for it are held: no new ones fire, and open ones neither repeat
//     nor resolve (a "resolved" mid-maintenance would be noise, and a real
//     problem that outlasts the work still alerts once it ends);
//   - rollouts don't offer it updates (installs already running finish).
//
// Fleet operators manage any maintenance. Client operators manage their own
// client's, which only ever covers that client's devices.

const (
	maxMaintenanceSpan = 30 * 24 * time.Hour
	// Ended windows are kept this long: reports leave them out of availability.
	maintenanceKeep = 100 * 24 * time.Hour
)

// maintenanceState works out whether a maintenance applies at now and, if
// so, until when.
func maintenanceState(m *api.Maintenance, now time.Time) (state string, until *time.Time) {
	switch {
	case now.Before(m.StartsAt):
		return "scheduled", nil
	case m.EndsAt != nil && !now.Before(*m.EndsAt):
		return "ended", nil
	}
	if m.Recurring() {
		sched := api.Strategy{Windows: m.Windows, Days: m.Days, TimeZone: m.TimeZone}
		if !rollout.WindowOpen(sched, now) {
			return "waiting", nil
		}
		end := rollout.WindowEnd(sched, now)
		if m.EndsAt != nil && m.EndsAt.Before(end) {
			end = *m.EndsAt
		}
		return "active", &end
	}
	return "active", m.EndsAt
}

func annotateMaintenance(m *api.Maintenance, now time.Time) {
	m.State, _ = maintenanceState(m, now)
	m.Active = m.State == "active"
}

// maintenanceCovers reports whether a maintenance applies to a device: its
// selector, and for a client's maintenance also the client's scope.
func maintenanceCovers(m *api.Maintenance, d *api.Device) bool {
	return rollout.Matches(m.Selector, d) && (scopeEmpty(m.ClientScope) || rollout.Matches(m.ClientScope, d))
}

// maintenanceNow is the set of maintenance windows active at one moment.
type maintenanceNow struct {
	active []*api.Maintenance
	until  map[string]*time.Time
}

func (s *Server) activeMaintenance(ctx context.Context, now time.Time) (*maintenanceNow, error) {
	all, err := s.store.ListMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	mn := &maintenanceNow{until: map[string]*time.Time{}}
	for _, m := range all {
		if state, until := maintenanceState(m, now); state == "active" {
			mn.active = append(mn.active, m)
			mn.until[m.ID] = until
		}
	}
	return mn, nil
}

// info describes the maintenance covering a device, or nil. With several,
// the flags combine and the latest end wins.
func (mn *maintenanceNow) info(d *api.Device) *api.MaintenanceInfo {
	if mn == nil {
		return nil
	}
	var out *api.MaintenanceInfo
	for _, m := range mn.active {
		if !maintenanceCovers(m, d) {
			continue
		}
		if out == nil {
			out = &api.MaintenanceInfo{ID: m.ID, Name: m.Name, Note: m.Note, Until: mn.until[m.ID]}
		} else if u := mn.until[m.ID]; u != nil && out.Until != nil && u.After(*out.Until) {
			out.Until = u
		}
		out.Alerts = out.Alerts || m.Holds()
		out.Update = out.Update || m.Pauses()
	}
	return out
}

func (mn *maintenanceNow) holdsAlerts(d *api.Device) bool {
	i := mn.info(d)
	return i != nil && i.Alerts
}

func (mn *maintenanceNow) empty() bool { return mn == nil || len(mn.active) == 0 }

// annotateDevices sets Device.Maintenance on each device. Errors leave the
// devices unannotated: this is display only.
func (s *Server) annotateDevices(ctx context.Context, devs ...*api.Device) {
	mn, err := s.activeMaintenance(ctx, s.now())
	if err != nil {
		s.log.Error("maintenance", "err", err)
		return
	}
	for _, d := range devs {
		d.Maintenance = mn.info(d)
	}
}

// maintenanceAccess decides who sees and manages a maintenance window.
func maintenanceAccess(p *principal, m *api.Maintenance) (see, edit bool) {
	if fr := p.fleetRole(); fr != "" {
		see, edit = true, api.RoleRank(fr) >= api.RoleRank(api.RoleOperator)
	}
	if !scopeEmpty(m.ClientScope) {
		for _, g := range p.grants {
			if sameScope(g.Scope, m.ClientScope) {
				see = true
				edit = edit || api.RoleRank(g.Role) >= api.RoleRank(api.RoleOperator)
			}
		}
	}
	return see, edit
}

func (s *Server) handleListMaintenance(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListMaintenance(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p, now := principalFrom(r.Context()), s.now()
	out := []*api.Maintenance{}
	for _, m := range all {
		if see, _ := maintenanceAccess(p, m); !see {
			continue
		}
		annotateMaintenance(m, now)
		if m.State == "ended" && r.URL.Query().Get("all") == "" && now.Sub(*m.EndsAt) > 24*time.Hour {
			continue
		}
		out = append(out, m)
	}
	// Active first, then upcoming, then the rest; newest first within each.
	rank := map[string]int{"active": 0, "waiting": 1, "scheduled": 2, "ended": 3}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].State] != rank[out[j].State] {
			return rank[out[i].State] < rank[out[j].State]
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateMaintenance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var m api.Maintenance
	if !readJSON(w, r, &m) {
		return
	}
	now := s.now()
	m.ID, m.CreatedAt, m.CreatedBy = newID("mnt"), now, ""
	if p := principalFrom(ctx); p != nil {
		m.CreatedBy = p.name
	}
	if code, err := s.checkMaintenance(ctx, principalFrom(ctx), &m, now); err != nil {
		writeError(w, code, err.Error())
		return
	}
	if err := s.store.PutMaintenance(ctx, &m); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "maintenance.created", "", "", fmt.Sprintf("maintenance %q created (%s)", m.Name, describeMaintenance(&m)))
	annotateMaintenance(&m, now)
	writeJSON(w, http.StatusCreated, m)
}

// checkMaintenance validates a new maintenance window and works out which
// client it belongs to.
func (s *Server) checkMaintenance(ctx context.Context, p *principal, m *api.Maintenance, now time.Time) (int, error) {
	bad := func(format string, a ...any) (int, error) { return http.StatusBadRequest, fmt.Errorf(format, a...) }
	m.Name, m.Note = strings.TrimSpace(m.Name), strings.TrimSpace(m.Note)
	if len(m.Name) > 100 || len(m.Note) > 1000 {
		return bad("name is limited to 100 characters and note to 1000")
	}
	if m.Selector.Channel == "" && len(m.Selector.Labels) == 0 && len(m.Selector.DeviceIDs) == 0 {
		return bad("say which devices: device_ids, labels or channel")
	}
	if len(m.Selector.DeviceIDs) > 1000 {
		return bad("at most 1000 device IDs; use labels for larger groups")
	}
	if m.StartsAt.IsZero() {
		m.StartsAt = now
	}
	if m.EndsAt != nil && !m.EndsAt.After(m.StartsAt) {
		return bad("ends_at must be after starts_at")
	}
	if m.EndsAt != nil && !m.EndsAt.After(now) {
		return bad("ends_at is in the past")
	}
	if m.Recurring() {
		sched := api.Strategy{Windows: m.Windows, Days: m.Days, TimeZone: m.TimeZone}
		if err := rollout.ValidateSchedule(&sched); err != nil {
			return bad("%v", err)
		}
		m.Days = sched.Days
	} else {
		if m.EndsAt == nil {
			return bad("a one-off maintenance needs ends_at (or make it recurring with windows/days)")
		}
		if m.EndsAt.Sub(m.StartsAt) > maxMaintenanceSpan {
			return bad("a one-off maintenance can last at most 30 days; use a recurring window for standing maintenance")
		}
	}
	if !m.Holds() && !m.Pauses() {
		return bad("a maintenance that neither holds alerts nor pauses updates does nothing")
	}

	// Devices named by ID must exist, and the caller must be an operator on each.
	devs := map[string]*api.Device{}
	for _, id := range m.Selector.DeviceIDs {
		d, err := s.store.GetDevice(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !p.sees(d)) {
			return bad("unknown device %s", id)
		} else if err != nil {
			return http.StatusInternalServerError, err
		}
		if api.RoleRank(p.roleFor(d)) < api.RoleRank(api.RoleOperator) {
			return http.StatusForbidden, fmt.Errorf("you are not an operator on %s", d.Name)
		}
		devs[id] = d
	}
	if m.Name == "" {
		switch {
		case len(devs) == 1:
			m.Name = "Maintenance on " + devs[m.Selector.DeviceIDs[0]].Name
		case len(devs) > 1:
			m.Name = fmt.Sprintf("Maintenance on %d devices", len(devs))
		default:
			m.Name = "Maintenance"
		}
	}

	// Which client is this for? Fleet operators may leave it empty (or name
	// any). Client operators get one of their scopes: the one they name,
	// their only one, or the only one covering all the named devices.
	if api.RoleRank(p.fleetRole()) >= api.RoleRank(api.RoleOperator) {
		return 0, nil
	}
	var cands []api.Selector
	for _, g := range p.grants {
		if scopeEmpty(g.Scope) || api.RoleRank(g.Role) < api.RoleRank(api.RoleOperator) {
			continue
		}
		if !scopeEmpty(m.ClientScope) && !sameScope(g.Scope, m.ClientScope) {
			continue
		}
		coversAll := true
		for _, d := range devs {
			coversAll = coversAll && rollout.Matches(g.Scope, d)
		}
		if coversAll {
			cands = append(cands, g.Scope)
		}
	}
	switch {
	case len(cands) == 0 && !scopeEmpty(m.ClientScope):
		return http.StatusForbidden, errors.New("you can only plan maintenance for clients where you are an operator")
	case len(cands) == 0:
		return http.StatusForbidden, errors.New("you need the operator role to plan maintenance (for all named devices, within one client)")
	case len(cands) > 1:
		return bad("say which client this is for (client_scope)")
	}
	m.ClientScope = cands[0]
	return 0, nil
}

func describeMaintenance(m *api.Maintenance) string {
	var parts []string
	if len(m.Selector.DeviceIDs) > 0 {
		parts = append(parts, fmt.Sprintf("%d devices", len(m.Selector.DeviceIDs)))
	}
	for k, v := range m.Selector.Labels {
		parts = append(parts, k+"="+v)
	}
	if m.Selector.Channel != "" {
		parts = append(parts, "channel "+m.Selector.Channel)
	}
	when := m.StartsAt.UTC().Format("2006-01-02 15:04")
	if m.EndsAt != nil {
		when += " to " + m.EndsAt.UTC().Format("2006-01-02 15:04") + " UTC"
	} else {
		when = "from " + when + " UTC"
	}
	if m.Recurring() {
		w := m.Windows
		if w == "" {
			w = "all day"
		}
		if len(m.Days) > 0 {
			w += " on " + strings.Join(m.Days, ",")
		}
		if m.TimeZone != "" {
			w += " " + m.TimeZone
		}
		when = w + ", " + when
	}
	var holds []string
	if m.Holds() {
		holds = append(holds, "alerts held")
	}
	if m.Pauses() {
		holds = append(holds, "updates paused")
	}
	return strings.Join(parts, " ") + "; " + when + "; " + strings.Join(holds, ", ")
}

// loadMaintenance loads the window in the path and checks the caller may change it.
func (s *Server) loadMaintenance(w http.ResponseWriter, r *http.Request) (*api.Maintenance, bool) {
	m, err := s.store.GetMaintenance(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "maintenance not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	see, edit := maintenanceAccess(principalFrom(r.Context()), m)
	if !see {
		writeError(w, http.StatusNotFound, "maintenance not found")
		return nil, false
	}
	if !edit {
		writeError(w, http.StatusForbidden, "you can't change this maintenance")
		return nil, false
	}
	return m, true
}

// handleEndMaintenance ends a window now (or cancels a scheduled one). It
// stays listed for a day.
func (s *Server) handleEndMaintenance(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMaintenance(w, r)
	if !ok {
		return
	}
	now := s.now()
	if state, _ := maintenanceState(m, now); state == "ended" {
		writeError(w, http.StatusConflict, "this maintenance has already ended")
		return
	}
	if m.StartsAt.After(now) {
		m.StartsAt = now
	}
	m.EndsAt = &now
	if err := s.store.PutMaintenance(r.Context(), m); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "maintenance.ended", "", "", fmt.Sprintf("maintenance %q ended", m.Name))
	if m.Pauses() {
		s.wakeMatching(r.Context(), m.Selector) // held updates can go out now
	}
	annotateMaintenance(m, now)
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDeleteMaintenance(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMaintenance(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMaintenance(r.Context(), m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "maintenance.deleted", "", "", fmt.Sprintf("maintenance %q deleted", m.Name))
	if m.Pauses() {
		s.wakeMatching(r.Context(), m.Selector) // held updates can go out now
	}
	w.WriteHeader(http.StatusNoContent)
}

// pruneMaintenance drops windows that ended long ago.
func (s *Server) pruneMaintenance(ctx context.Context, now time.Time) error {
	all, err := s.store.ListMaintenance(ctx)
	if err != nil {
		return err
	}
	for _, m := range all {
		if m.EndsAt != nil && now.Sub(*m.EndsAt) > maintenanceKeep {
			if err := s.store.DeleteMaintenance(ctx, m.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

// ---- acknowledgement ----

// loadAlertForAck loads the alert in the path and checks the caller may
// acknowledge it: operator on its device, or fleet operator for alerts
// about a rollout.
func (s *Server) loadAlertForAck(w http.ResponseWriter, r *http.Request) (*api.Alert, bool) {
	ctx := r.Context()
	a, err := s.store.GetAlert(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "alert not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	p := principalFrom(ctx)
	if len(s.visibleAlerts(ctx, p, []*api.Alert{a})) == 0 {
		writeError(w, http.StatusNotFound, "alert not found")
		return nil, false
	}
	var d *api.Device
	if a.DeviceID != "" && a.DeploymentID == "" {
		if d, err = s.store.GetDevice(ctx, a.DeviceID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, err)
			return nil, false
		}
	}
	role := p.fleetRole()
	if d != nil {
		role = p.roleFor(d)
	}
	if api.RoleRank(role) < api.RoleRank(api.RoleOperator) {
		writeError(w, http.StatusForbidden, "acknowledging this alert needs the operator role")
		return nil, false
	}
	if a.Status != api.AlertFiring {
		writeError(w, http.StatusConflict, "this alert is no longer firing")
		return nil, false
	}
	return a, true
}

func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	var req api.AckRequest
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if len(req.Note) > 500 {
		writeError(w, http.StatusBadRequest, "note is limited to 500 characters")
		return
	}
	a, ok := s.loadAlertForAck(w, r)
	if !ok {
		return
	}
	now := s.now()
	a.AckedBy, a.AckedAt, a.AckNote = principalFrom(r.Context()).name, &now, req.Note
	if err := s.store.PutAlert(r.Context(), a); err != nil {
		s.internalError(w, err)
		return
	}
	msg := "acknowledged: " + a.RuleName + " — " + a.Summary
	if req.Note != "" {
		msg += " (" + req.Note + ")"
	}
	s.logEvent(r.Context(), "alert.acknowledged", a.DeviceID, a.DeploymentID, msg)
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleUnackAlert(w http.ResponseWriter, r *http.Request) {
	a, ok := s.loadAlertForAck(w, r)
	if !ok {
		return
	}
	if a.AckedAt == nil {
		writeJSON(w, http.StatusOK, a)
		return
	}
	a.AckedBy, a.AckedAt, a.AckNote = "", nil, ""
	if err := s.store.PutAlert(r.Context(), a); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "alert.unacknowledged", a.DeviceID, a.DeploymentID, "acknowledgement removed: "+a.RuleName+" — "+a.Summary)
	writeJSON(w, http.StatusOK, a)
}
