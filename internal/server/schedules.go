package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Scheduled actions run a bulk operation at a time of day on chosen days.
// Each run picks its devices afresh from the selector, holds their alerts
// for a while (a maintenance window) and queues the action like a bulk
// operation, so its progress shows on the Bulk page.

const (
	scheduleDefaultHold = 30        // minutes
	scheduleMaxHold     = 24 * 60   // minutes
	scheduleMissedAfter = time.Hour // later than this, a run is skipped as missed
	maxScheduledActions = 200
)

var scheduleOperations = []string{api.ActionReboot, api.ActionRestartAgent, api.ActionLock, api.BulkWake, api.ActionNetCheck, api.ActionExec}

// actionPrincipal is who a scheduled action acts as: an operator on exactly its
// client's devices, or on the whole fleet for the vendor's.
func actionPrincipal(sa *api.ScheduledAction) *principal {
	return &principal{name: "scheduled action " + sa.Name, role: api.RoleOperator,
		grants: []api.Grant{{Role: api.RoleOperator, Scope: sa.ClientScope}}}
}

func actionLocation(sa *api.ScheduledAction) *time.Location {
	if sa.TimeZone != "" {
		if loc, err := time.LoadLocation(sa.TimeZone); err == nil {
			return loc
		}
	}
	return time.UTC
}

func scheduleBulkRequest(sa *api.ScheduledAction, dryRun bool) *api.BulkRequest {
	return &api.BulkRequest{Operation: sa.Operation, Selector: sa.Selector, All: sa.All,
		Args: slices.Clone(sa.Args), DryRun: dryRun, ClientScope: sa.ClientScope}
}

// checkSchedule validates a new or edited schedule as p and works out its
// client and next run.
func (s *Server) checkSchedule(ctx context.Context, p *principal, sa *api.ScheduledAction, client string, now time.Time) (int, error) {
	bad := func(format string, a ...any) (int, error) { return http.StatusBadRequest, fmt.Errorf(format, a...) }
	sa.Name = strings.TrimSpace(sa.Name)
	if sa.Name == "" || len(sa.Name) > 100 {
		return bad("give it a name (up to 100 characters)")
	}
	if !slices.Contains(scheduleOperations, sa.Operation) {
		return bad("operation must be one of %s", strings.Join(scheduleOperations, ", "))
	}
	if scopeEmpty(sa.Selector) && len(sa.Selector.DeviceIDs) == 0 && !sa.All {
		return bad("say which devices (labels, channel or device_ids), or all=true")
	}
	days, err := rollout.ParseDays(sa.Days)
	if err != nil {
		return bad("%v", err)
	}
	sa.Days = days
	if sa.TimeZone != "" {
		if _, err := time.LoadLocation(sa.TimeZone); err != nil {
			return bad("unknown time zone %q (use an IANA name such as Asia/Singapore)", sa.TimeZone)
		}
	}
	next, err := rollout.NextDailyRun(sa.At, sa.Days, actionLocation(sa), now)
	if err != nil {
		return bad("%v", err)
	}
	if sa.HoldAlertsMinutes == nil {
		h := scheduleDefaultHold
		sa.HoldAlertsMinutes = &h
	}
	if *sa.HoldAlertsMinutes < 0 || *sa.HoldAlertsMinutes > scheduleMaxHold {
		return bad("hold_alerts_minutes must be 0 to %d", scheduleMaxHold)
	}
	if sa.ClientScope, err = chooseClientScope(p, client, "scheduled actions"); err != nil {
		return http.StatusForbidden, err
	}
	if sa.Operation == api.ActionExec && (!scopeEmpty(sa.ClientScope) || api.RoleRank(p.fleetRole()) < api.RoleRank(api.RoleOperator)) {
		return http.StatusForbidden, errors.New("only fleet-wide operators can schedule commands (exec), and not for a client")
	}
	// The same checks a bulk operation makes (e.g. exec needs a command).
	b, code, err := s.runBulk(ctx, actionPrincipal(sa), scheduleBulkRequest(sa, true), false)
	if err != nil {
		return code, err
	}
	sa.Matches = len(b.Applied)
	if sa.Enabled {
		sa.NextRunAt = &next
	} else {
		sa.NextRunAt = nil
	}
	return 0, nil
}

// runSchedule runs a scheduled action now.
func (s *Server) runSchedule(ctx context.Context, sa *api.ScheduledAction, now time.Time, manual bool) *api.ScheduledRun {
	run := &api.ScheduledRun{At: now, Manual: manual}
	p := actionPrincipal(sa)
	ctx = withPrincipal(ctx, p)
	preview, _, err := s.runBulk(ctx, p, scheduleBulkRequest(sa, true), false)
	if err == nil && len(preview.Applied) > 0 && sa.HoldAlertsMinutes != nil && *sa.HoldAlertsMinutes > 0 {
		end := now.Add(time.Duration(*sa.HoldAlertsMinutes) * time.Minute)
		suppress, pause := true, false
		m := &api.Maintenance{ID: newID("mnt"), Name: "Scheduled " + sa.Operation + ": " + sa.Name,
			Note:     "held while scheduled action " + sa.Name + " runs",
			Selector: api.Selector{DeviceIDs: targetIDs(preview.Applied)}, StartsAt: now, EndsAt: &end,
			SuppressAlerts: &suppress, PauseUpdates: &pause, ClientScope: sa.ClientScope, CreatedBy: p.name, CreatedAt: now}
		if err := s.store.PutMaintenance(ctx, m); err != nil {
			s.log.Error("scheduled action maintenance", "schedule", sa.ID, "err", err)
		}
	}
	if err == nil {
		var b *api.BulkOperation
		if b, _, err = s.runBulk(ctx, p, scheduleBulkRequest(sa, false), false); err == nil {
			run.BulkID, run.Applied, run.Skipped = b.ID, len(b.Applied), len(b.Skipped)
		}
	}
	if err != nil {
		run.Error = err.Error()
		s.logEvent(ctx, "schedule.failed", "", "", fmt.Sprintf("scheduled action %q failed: %v", sa.Name, err))
	} else {
		s.logEvent(ctx, "schedule.ran", "", "", fmt.Sprintf("scheduled action %q: %s on %s (%d skipped) — %s",
			sa.Name, sa.Operation, plural(run.Applied, "device"), run.Skipped, run.BulkID))
	}
	return run
}

func targetIDs(ts []api.BulkTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}

// scheduleTick runs the scheduled actions that are due. A run more than
// an hour late (the server was down) is recorded as missed, not run.
func (s *Server) scheduleTick(ctx context.Context, now time.Time) error {
	all, err := s.store.ListScheduledActions(ctx)
	if err != nil {
		return err
	}
	for _, sa := range all {
		if !sa.Enabled || sa.NextRunAt == nil || now.Before(*sa.NextRunAt) {
			continue
		}
		var run *api.ScheduledRun
		if now.Sub(*sa.NextRunAt) > scheduleMissedAfter {
			run = &api.ScheduledRun{At: *sa.NextRunAt, Missed: true, Error: "missed: the server wasn't running then"}
			s.logEvent(ctx, "schedule.missed", "", "", fmt.Sprintf("scheduled action %q missed its %s run", sa.Name, sa.NextRunAt.Format(time.RFC3339)))
		} else {
			run = s.runSchedule(ctx, sa, now, false)
		}
		if err := s.recordScheduleRun(ctx, sa.ID, run, now); err != nil {
			return err
		}
	}
	return nil
}

// recordScheduleRun stores a run's outcome and the next run time, from the
// schedule as it is now (it may have been edited, paused or deleted while
// the run went on).
func (s *Server) recordScheduleRun(ctx context.Context, id string, run *api.ScheduledRun, now time.Time) error {
	err := s.store.UpdateScheduledAction(ctx, id, func(cur *api.ScheduledAction) {
		cur.LastRun, cur.NextRunAt = run, nil
		if cur.Enabled {
			if next, err := rollout.NextDailyRun(cur.At, cur.Days, actionLocation(cur), now); err == nil {
				cur.NextRunAt = &next
			}
		}
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil // deleted meanwhile: nothing to record
	}
	return err
}

// countScheduleMatches fills in how many devices each schedule would act on.
func (s *Server) countScheduleMatches(ctx context.Context, sas []*api.ScheduledAction) error {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return err
	}
	for _, sa := range sas {
		p := actionPrincipal(sa)
		sa.Matches = 0
		for _, d := range devs {
			if p.sees(d) && rollout.Matches(sa.Selector, d) && d.Lifecycle != api.LifecycleRetired {
				sa.Matches++
			}
		}
	}
	return nil
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListScheduledActions(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.ScheduledAction{}
	for _, sa := range all {
		if see, _ := clientObjectAccess(p, sa.ClientScope); see {
			out = append(out, sa)
		}
	}
	if err := s.countScheduleMatches(r.Context(), out); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var sa api.ScheduledAction
	if !readJSON(w, r, &sa) {
		return
	}
	existing, err := s.store.ListScheduledActions(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(existing) >= maxScheduledActions {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d scheduled actions", maxScheduledActions))
		return
	}
	p, now := principalFrom(ctx), s.now()
	sa.ID, sa.CreatedBy, sa.CreatedAt, sa.LastRun = newID("sch"), p.name, now, nil
	if code, err := s.checkSchedule(ctx, p, &sa, clientParam(r, &sa), now); err != nil {
		writeError(w, code, err.Error())
		return
	}
	if err := s.store.PutScheduledAction(ctx, &sa); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "schedule.created", "", "", fmt.Sprintf("scheduled action %q created: %s", sa.Name, describeSchedule(&sa)))
	writeJSON(w, http.StatusCreated, sa)
}

// clientParam: a schedule's client comes from ?client=K=V or its client_scope labels.
func clientParam(r *http.Request, sa *api.ScheduledAction) string {
	if c := r.URL.Query().Get("client"); c != "" {
		return c
	}
	var parts []string
	for k, v := range sa.ClientScope.Labels {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ";")
}

func describeSchedule(sa *api.ScheduledAction) string {
	when := "daily"
	if len(sa.Days) > 0 && len(sa.Days) < 7 {
		when = strings.Join(sa.Days, ",")
	}
	tz := sa.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	target := describeSelector(sa.Selector)
	if target == "" {
		target = "all devices"
	}
	return fmt.Sprintf("%s %s at %s %s on %s", sa.Operation, when, sa.At, tz, target)
}

func describeSelector(sel api.Selector) string {
	var parts []string
	if len(sel.DeviceIDs) > 0 {
		parts = append(parts, plural(len(sel.DeviceIDs), "device"))
	}
	if sel.Channel != "" {
		parts = append(parts, "channel "+sel.Channel)
	}
	if len(sel.Labels) > 0 {
		parts = append(parts, fmtSelectorLabels(sel.Labels))
	}
	return strings.Join(parts, ", ")
}

// loadSchedule loads a schedule the caller can see; edit says they must
// be able to change it.
func (s *Server) loadSchedule(w http.ResponseWriter, r *http.Request, edit bool) (*api.ScheduledAction, bool) {
	sa, err := s.store.GetScheduledAction(r.Context(), r.PathValue("id"))
	if err == nil {
		see, canEdit := clientObjectAccess(principalFrom(r.Context()), sa.ClientScope)
		switch {
		case !see:
			err = store.ErrNotFound
		case edit && !canEdit:
			writeError(w, http.StatusForbidden, "you can't change this scheduled action")
			return nil, false
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "scheduled action not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return sa, true
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	old, ok := s.loadSchedule(w, r, true)
	if !ok {
		return
	}
	var sa api.ScheduledAction
	if !readJSON(w, r, &sa) {
		return
	}
	sa.ID, sa.CreatedBy, sa.CreatedAt, sa.LastRun = old.ID, old.CreatedBy, old.CreatedAt, old.LastRun
	client := clientParam(r, &sa)
	if client == "" {
		client = clientParam(r, old) // stays with its client
	}
	p, now := principalFrom(ctx), s.now()
	if code, err := s.checkSchedule(ctx, p, &sa, client, now); err != nil {
		writeError(w, code, err.Error())
		return
	}
	if err := s.store.PutScheduledAction(ctx, &sa); err != nil {
		s.internalError(w, err)
		return
	}
	state := "updated"
	if old.Enabled != sa.Enabled {
		state = map[bool]string{true: "enabled", false: "paused"}[sa.Enabled]
	}
	s.logEvent(ctx, "schedule.updated", "", "", fmt.Sprintf("scheduled action %q %s: %s", sa.Name, state, describeSchedule(&sa)))
	writeJSON(w, http.StatusOK, sa)
}

func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sa, ok := s.loadSchedule(w, r, true)
	if !ok {
		return
	}
	who := principalFrom(ctx).name
	s.logEvent(ctx, "schedule.run_now", "", "", fmt.Sprintf("scheduled action %q run now by %s", sa.Name, who))
	run := s.runSchedule(context.WithoutCancel(ctx), sa, s.now(), true)
	if err := s.store.UpdateScheduledAction(ctx, sa.ID, func(cur *api.ScheduledAction) {
		cur.LastRun = run
		*sa = *cur
	}); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the scheduled action was deleted while it ran")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.countScheduleMatches(ctx, []*api.ScheduledAction{sa}); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sa)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	sa, ok := s.loadSchedule(w, r, true)
	if !ok {
		return
	}
	if err := s.store.DeleteScheduledAction(r.Context(), sa.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "schedule.deleted", "", "", fmt.Sprintf("scheduled action %q deleted", sa.Name))
	w.WriteHeader(http.StatusNoContent)
}
