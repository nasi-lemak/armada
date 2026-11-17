package server

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Customer offboarding: when a customer leaves, export everything Armada
// holds about them in one file, then remove their devices, accounts and
// settings in one step. A customer is a set of labels (customer=acme).
//
// Something belongs to the customer when its scope includes all of those
// labels: devices with the labels; accounts, grants and group mappings
// limited to them; and alert rules, channels, webhooks, tokens,
// pre-registrations, schedules, maintenance windows, report schedules and
// certificate policies made for them.

type offboardItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// offboardPlan is what offboarding a customer removes.
type offboardPlan struct {
	Labels       map[string]string `json:"labels"`
	Confirm      string            `json:"confirm"` // type this to run it
	DeviceAction string            `json:"device_action"`
	Devices      []offboardItem    `json:"devices"`
	// Users are deleted; UsersNarrowed keep access to other customers and
	// lose only this one.
	Users          []offboardItem `json:"users"`
	UsersNarrowed  []offboardItem `json:"users_narrowed"`
	GroupMappings  []offboardItem `json:"group_mappings"`
	Tokens         []offboardItem `json:"tokens"`
	AlertRules     []offboardItem `json:"alert_rules"`
	Channels       []offboardItem `json:"channels"`
	EventHooks     []offboardItem `json:"event_hooks"`
	Registrations  []offboardItem `json:"registrations"`
	Schedules      []offboardItem `json:"scheduled_actions"`
	Maintenance    []offboardItem `json:"maintenance"`
	ReportSchedule []offboardItem `json:"report_schedules"`
	CertPolicies   []offboardItem `json:"cert_policies"`
	Done           bool           `json:"done"`
}

type offboardRequest struct {
	Labels map[string]string `json:"labels"`
	// DeviceAction is "delete" (default: gone, keys stop working) or
	// "retire" (kept as retired records, keys stop working).
	DeviceAction string `json:"device_action"`
	// Confirm must be the plan's confirm text; without it nothing changes
	// and the plan is returned.
	Confirm string `json:"confirm"`
}

// within reports whether scope is limited to the customer: it requires
// every one of the customer's labels.
func within(scope api.Selector, labels map[string]string) bool {
	if len(labels) == 0 || len(scope.Labels) == 0 {
		return false
	}
	for k, v := range labels {
		if scope.Labels[k] != v {
			return false
		}
	}
	return true
}

func labelsText(labels map[string]string) string {
	var parts []string
	for _, kv := range sortedPairs(labels) {
		parts = append(parts, kv[0]+"="+kv[1])
	}
	return strings.Join(parts, ",")
}

func sortedPairs(m map[string]string) [][2]string {
	out := make([][2]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, [2]string{k, m[k]})
	}
	return out
}

// offboardTargets finds what belongs to the customer.
type offboardTargets struct {
	plan     offboardPlan
	devices  []*api.Device
	users    []*api.User
	narrowed []*api.User // with the grants they keep
}

func (s *Server) offboardTargets(ctx context.Context, labels map[string]string, deviceAction string) (*offboardTargets, error) {
	t := &offboardTargets{plan: offboardPlan{Labels: labels, Confirm: labelsText(labels), DeviceAction: deviceAction}}
	p := &t.plan
	sel := api.Selector{Labels: labels}
	devs, err := s.store.ListAllDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		if rollout.Matches(sel, d) {
			t.devices = append(t.devices, d)
			p.Devices = append(p.Devices, offboardItem{d.ID, d.Name})
		}
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.GroupManaged {
			continue // access comes from group mappings, handled below
		}
		var keep []api.Grant
		lost := false
		for _, g := range userGrants(u) {
			if within(g.Scope, labels) {
				lost = true
			} else {
				keep = append(keep, g)
			}
		}
		switch {
		case !lost:
		case len(keep) == 0:
			t.users = append(t.users, u)
			p.Users = append(p.Users, offboardItem{u.ID, u.Email})
		default:
			nu := *u
			nu.Grants = keep
			t.narrowed = append(t.narrowed, &nu)
			p.UsersNarrowed = append(p.UsersNarrowed, offboardItem{u.ID, u.Email})
		}
	}
	add := func(list *[]offboardItem, ok bool, id, name string) {
		if ok {
			*list = append(*list, offboardItem{id, name})
		}
	}
	maps, err := s.store.ListGroupMappings(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range maps {
		add(&p.GroupMappings, within(m.Scope, labels), m.ID, m.Group)
	}
	toks, err := s.store.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	for _, tk := range toks {
		add(&p.Tokens, !tk.Revoked && (within(tk.ClientScope, labels) || within(api.Selector{Labels: tk.Labels}, labels)), tk.ID, tk.Description)
	}
	rules, err := s.store.ListAlertRules(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		add(&p.AlertRules, within(r.ClientScope, labels), r.ID, r.Name)
	}
	chans, err := s.store.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chans {
		add(&p.Channels, within(c.ClientScope, labels), c.ID, c.Name)
	}
	hooks, err := s.store.ListEventHooks(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hooks {
		add(&p.EventHooks, within(h.ClientScope, labels), h.ID, h.Name)
	}
	regs, err := s.store.ListRegistrations(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range regs {
		add(&p.Registrations, within(r.ClientScope, labels) || within(api.Selector{Labels: r.Labels}, labels), r.Serial, r.Name)
	}
	scheds, err := s.store.ListScheduledActions(ctx)
	if err != nil {
		return nil, err
	}
	for _, sa := range scheds {
		add(&p.Schedules, within(sa.ClientScope, labels) || within(sa.Selector, labels), sa.ID, sa.Name)
	}
	mws, err := s.store.ListMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range mws {
		add(&p.Maintenance, within(m.ClientScope, labels) || within(m.Selector, labels), m.ID, m.Name)
	}
	reps, err := s.store.ListReportSchedules(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range reps {
		add(&p.ReportSchedule, within(r.ClientScope, labels), r.ID, r.Name)
	}
	pols, err := s.store.ListCertPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range pols {
		add(&p.CertPolicies, within(c.ClientScope, labels), c.ID, c.Name)
	}
	return t, nil
}

func readOffboardLabels(w http.ResponseWriter, labels map[string]string) bool {
	if len(labels) == 0 {
		writeError(w, http.StatusBadRequest, "say which customer: labels, e.g. {\"customer\": \"acme\"}")
		return false
	}
	for k, v := range labels {
		if k == "" || v == "" {
			writeError(w, http.StatusBadRequest, "labels need a key and a value")
			return false
		}
	}
	return true
}

// handleOffboard returns the plan, or runs it when confirm matches.
func (s *Server) handleOffboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req offboardRequest
	if !readJSON(w, r, &req) || !readOffboardLabels(w, req.Labels) {
		return
	}
	switch req.DeviceAction {
	case "":
		req.DeviceAction = "delete"
	case "delete", "retire":
	default:
		writeError(w, http.StatusBadRequest, "device_action is delete or retire")
		return
	}
	t, err := s.offboardTargets(ctx, req.Labels, req.DeviceAction)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if req.Confirm == "" {
		writeJSON(w, http.StatusOK, t.plan)
		return
	}
	if req.Confirm != t.plan.Confirm {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("to offboard, confirm with %q", t.plan.Confirm))
		return
	}
	if err := s.runOffboard(ctx, t); err != nil {
		s.internalError(w, err)
		return
	}
	t.plan.Done = true
	writeJSON(w, http.StatusOK, t.plan)
}

func (s *Server) runOffboard(ctx context.Context, t *offboardTargets) error {
	p := &t.plan
	by := principalFrom(ctx).name
	now := s.now()
	for _, d := range t.devices {
		if p.DeviceAction == "retire" {
			if d.Lifecycle == api.LifecycleRetired {
				continue
			}
			d.Lifecycle = api.LifecycleRetired
			d.LifecycleInfo = &api.LifecycleInfo{Note: "customer offboarded", ChangedAt: now, ChangedBy: by}
			if err := s.store.SetDeviceLifecycle(ctx, d); err != nil {
				return err
			}
			s.logEvent(ctx, "device.retired", d.ID, "", fmt.Sprintf("%s retired: customer %s offboarded", d.Name, p.Confirm))
			continue
		}
		if err := s.store.DeleteDevice(ctx, d.ID); err != nil && err != store.ErrNotFound {
			return err
		}
		s.logEvent(ctx, "device.deleted", "", "", fmt.Sprintf("%s (%s) removed: customer %s offboarded", d.Name, d.ID, p.Confirm))
	}
	for _, u := range t.users {
		if err := s.store.DeleteUser(ctx, u.ID); err != nil && err != store.ErrNotFound {
			return err
		}
	}
	for _, u := range t.narrowed {
		if err := s.store.UpdateUser(ctx, u); err != nil {
			return err
		}
	}
	type del struct {
		items []offboardItem
		fn    func(context.Context, string) error
	}
	for _, d := range []del{
		{p.GroupMappings, s.store.DeleteGroupMapping},
		{p.Tokens, s.store.RevokeToken},
		{p.AlertRules, s.store.DeleteAlertRule},
		{p.Channels, s.store.DeleteChannel},
		{p.EventHooks, s.store.DeleteEventHook},
		{p.Registrations, s.store.DeleteRegistration},
		{p.Schedules, s.store.DeleteScheduledAction},
		{p.Maintenance, s.store.DeleteMaintenance},
		{p.ReportSchedule, s.store.DeleteReportSchedule},
		{p.CertPolicies, s.store.DeleteCertPolicy},
	} {
		for _, it := range d.items {
			if err := d.fn(ctx, it.ID); err != nil && err != store.ErrNotFound {
				return err
			}
		}
	}
	if len(p.EventHooks) > 0 {
		s.invalidateHooks()
	}
	s.logEvent(ctx, "customer.offboarded", "", "", fmt.Sprintf("customer %s offboarded: %d devices %s, %d accounts deleted, %d narrowed, %d group mappings, %d tokens, %d alert rules, %d channels, %d webhooks, %d pre-registrations, %d schedules, %d maintenance windows, %d report schedules, %d certificate policies removed",
		p.Confirm, len(p.Devices), map[string]string{"delete": "deleted", "retire": "retired"}[p.DeviceAction], len(p.Users), len(p.UsersNarrowed),
		len(p.GroupMappings), len(p.Tokens), len(p.AlertRules), len(p.Channels), len(p.EventHooks), len(p.Registrations), len(p.Schedules),
		len(p.Maintenance), len(p.ReportSchedule), len(p.CertPolicies)))
	return nil
}

// ---- export ----

// handleOffboardExport writes a zip of everything about the customer:
// devices with inventory, health history, alerts, disconnections,
// certificates and activity; accounts; and the customer's settings.
// Secrets (passwords, tokens, webhook secrets) are left out.
func (s *Server) handleOffboardExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	labels, err := parseLabelList(r.URL.Query().Get("labels"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "labels: "+err.Error())
		return
	}
	if !readOffboardLabels(w, labels) {
		return
	}
	t, err := s.offboardTargets(ctx, labels, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	now := s.now()
	name := "armada-export-" + fileSafe(t.plan.Confirm) + "-" + now.Format("2006-01-02") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	put := func(file string, v any) error {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: file, Method: zip.Deflate, Modified: now})
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	from := now.Add(-s.cfg.HistoryRetention)
	type deviceExport struct {
		Device       *api.Device         `json:"device"`
		Updates      []*api.DeviceUpdate `json:"updates"`
		Alerts       []*api.Alert        `json:"alerts"`
		Outages      []*api.Outage       `json:"disconnections"`
		Certificates []*api.DeviceCert   `json:"certificates"`
		History      []api.HistorySeries `json:"health_history_hourly"`
	}
	ids := map[string]bool{}
	for _, d := range t.devices {
		ids[d.ID] = true
		s.decorate(d)
		ex := deviceExport{Device: d}
		if ex.Updates, err = s.store.ListDeviceUpdates(ctx, "", d.ID); err == nil {
			if ex.Alerts, err = s.store.ListDeviceAlerts(ctx, d.ID, now, 10000); err == nil {
				if ex.Outages, err = s.store.ListOutages(ctx, from, now, d.ID); err == nil {
					if ex.Certificates, err = s.store.ListDeviceCerts(ctx, d.ID); err == nil {
						ex.History, _, _, err = s.store.DeviceHistory(ctx, d.ID, from, true)
					}
				}
			}
		}
		if err == nil {
			err = put("devices/"+fileSafe(d.Name)+"_"+d.ID+".json", ex)
		}
		if err != nil {
			s.log.Error("offboarding export", "device", d.ID, "err", err)
			return // the zip is cut short; the client sees a broken download
		}
	}
	users := append([]*api.User{}, t.users...)
	users = append(users, t.narrowed...)
	settings := map[string]any{}
	if chans, err := s.store.ListChannels(ctx); err == nil {
		var mine []api.NotificationChannel
		for _, c := range chans {
			if within(c.ClientScope, labels) {
				mine = append(mine, redactChannel(*c))
			}
		}
		settings["channels"] = mine
	}
	if hooks, err := s.store.ListEventHooks(ctx); err == nil {
		var mine []*api.EventHook
		for _, h := range hooks {
			if within(h.ClientScope, labels) {
				mine = append(mine, redactHook(h))
			}
		}
		settings["event_hooks"] = mine
	}
	if rules, err := s.store.ListAlertRules(ctx); err == nil {
		var mine []*api.AlertRule
		for _, r := range rules {
			if within(r.ClientScope, labels) {
				mine = append(mine, r)
			}
		}
		settings["alert_rules"] = mine
	}
	if regs, err := s.store.ListRegistrations(ctx); err == nil {
		var mine []*api.Registration
		for _, r := range regs {
			if within(r.ClientScope, labels) || within(api.Selector{Labels: r.Labels}, labels) {
				mine = append(mine, r)
			}
		}
		settings["pre_registrations"] = mine
	}
	// The activity concerning the customer: about its devices, or done by
	// its accounts.
	emails := map[string]bool{}
	for _, u := range t.users {
		emails[u.Email] = true
	}
	f, err := zw.CreateHeader(&zip.FileHeader{Name: "activity.csv", Method: zip.Deflate, Modified: now})
	if err != nil {
		return
	}
	cw := csv.NewWriter(f)
	cw.Write([]string{"id", "time", "type", "device_id", "actor", "message", "hash"})
	events := 0
	before := int64(0)
	for {
		evs, err := s.store.QueryEvents(ctx, store.EventFilter{BeforeID: before}, 5000)
		if err != nil || len(evs) == 0 {
			break
		}
		for _, e := range evs {
			if ids[e.DeviceID] || emails[e.Actor] {
				cw.Write([]string{fmt.Sprint(e.ID), e.Time.UTC().Format(time.RFC3339), e.Type, e.DeviceID, e.Actor, e.Message, e.Hash})
				events++
			}
			before = e.ID
		}
	}
	cw.Flush()
	t.plan.Confirm = ""
	put("accounts.json", users)
	put("settings.json", settings)
	put("manifest.json", map[string]any{
		"customer": labels, "exported_at": now, "exported_by": principalFrom(ctx).name, "server_version": s.cfg.Version,
		"devices": len(t.devices), "accounts": len(users), "activity_events": events,
		"history_since": from, "would_be_removed": t.plan,
		"notes": "Device keys, passwords, tokens and webhook secrets are not included. Accounts listed with other customers keep those.",
	})
	s.logEvent(ctx, "customer.exported", "", "", fmt.Sprintf("export of customer %s: %d devices, %d accounts, %d activity events", labelsText(labels), len(t.devices), len(users), events))
}
