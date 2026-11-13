package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// eventAlertWindow is how long an event-based alert (update failures) stays
// open, folding further occurrences into one notification.
const eventAlertWindow = time.Hour

type candidate struct {
	rule     *api.AlertRule
	key      string
	severity string
	summary  string
	device   *api.Device
	depID    string
}

// evaluateAlerts fires and resolves state-based alerts. Called every engine tick.
func (s *Server) evaluateAlerts(ctx context.Context, now time.Time) error {
	rules, err := s.store.ListAlertRules(ctx)
	if err != nil {
		return err
	}
	ruleByID := map[string]*api.AlertRule{}
	needDevices, needDeployments := false, false
	for _, r := range rules {
		ruleByID[r.ID] = r
		if !r.Enabled {
			continue
		}
		switch r.Condition {
		case api.ConditionDeviceOffline, api.ConditionHealth:
			needDevices = true
		case api.ConditionDeploymentPaused:
			needDeployments = true
		}
	}
	var devices []*api.Device
	if needDevices {
		if devices, err = s.store.ListDevices(ctx); err != nil {
			return err
		}
	}
	var deps []*api.Deployment
	if needDeployments {
		if deps, err = s.store.ListDeployments(ctx, api.DeploymentPaused); err != nil {
			return err
		}
	}

	mn, err := s.activeMaintenance(ctx, now)
	if err != nil {
		return err
	}
	if !mn.empty() && devices == nil {
		if devices, err = s.store.ListDevices(ctx); err != nil {
			return err
		}
	}
	// held: devices in maintenance that holds alerts.
	held := map[string]bool{}
	if !mn.empty() {
		for _, d := range devices {
			if mn.holdsAlerts(d) {
				held[d.ID] = true
			}
		}
	}

	want := map[string]candidate{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		switch r.Condition {
		case api.ConditionDeviceOffline:
			limit := time.Duration(max(r.OfflineMinutes, 1)) * time.Minute
			if r.OfflineMinutes == 0 {
				limit = 10 * time.Minute
			}
			for _, d := range devices {
				if !ruleCovers(r, d) || d.LastSeenAt.IsZero() || held[d.ID] || !d.Alerting() {
					continue
				}
				if gone := now.Sub(d.LastSeenAt); gone >= limit {
					want[r.ID+"/"+d.ID] = candidate{rule: r, key: r.ID + "/" + d.ID, severity: api.HealthCritical, device: d,
						summary: fmt.Sprintf("%s has not checked in for %s", d.Name, roundDuration(gone))}
				}
			}
		case api.ConditionHealth:
			minRank := api.HealthRank(api.HealthCritical)
			if r.MinSeverity == api.HealthWarning {
				minRank = api.HealthRank(api.HealthWarning)
			}
			for _, d := range devices {
				if !ruleCovers(r, d) || held[d.ID] || !d.Alerting() {
					continue
				}
				var failing []string
				worst := api.HealthOK
				for _, c := range d.Checks {
					if api.HealthRank(c.Status) < minRank || (len(r.CheckKinds) > 0 && !containsStr(r.CheckKinds, c.Kind)) {
						continue
					}
					failing = append(failing, fmt.Sprintf("%s (%s): %s", c.Name, c.Status, c.Message))
					if api.HealthRank(c.Status) > api.HealthRank(worst) {
						worst = c.Status
					}
				}
				if len(failing) > 0 {
					sort.Strings(failing)
					want[r.ID+"/"+d.ID] = candidate{rule: r, key: r.ID + "/" + d.ID, severity: worst, device: d,
						summary: fmt.Sprintf("%s: %s", d.Name, strings.Join(failing, "; "))}
				}
			}
		case api.ConditionDeploymentPaused:
			if !scopeEmpty(r.ClientScope) {
				continue // fleet-level: never for client rules
			}
			for _, dep := range deps {
				want[r.ID+"/"+dep.ID] = candidate{rule: r, key: r.ID + "/" + dep.ID, severity: api.HealthCritical, depID: dep.ID,
					summary: fmt.Sprintf("Rollout %q (%s %s) is paused: %s", dep.Name, dep.Package, dep.Version, dep.StatusReason)}
			}
		}
	}

	active, err := s.store.ListAlerts(ctx, true, 10000)
	if err != nil {
		return err
	}
	activeByKey := map[string]*api.Alert{}
	for _, a := range active {
		activeByKey[a.Key] = a
	}

	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := want[k]
		a := activeByKey[k]
		if a == nil {
			a = &api.Alert{ID: newID("alr"), RuleID: c.rule.ID, RuleName: c.rule.Name, Condition: c.rule.Condition, Key: k,
				Status: api.AlertFiring, Severity: c.severity, Summary: c.summary, DeploymentID: c.depID,
				StartedAt: now, UpdatedAt: now, Count: 1}
			if c.device != nil {
				a.DeviceID, a.DeviceName = c.device.ID, c.device.Name
			}
			if err := s.fire(ctx, c.rule, a, c.device, now); err != nil {
				return err
			}
			continue
		}
		escalated := api.HealthRank(c.severity) > api.HealthRank(a.Severity)
		a.Severity, a.Summary, a.UpdatedAt = c.severity, c.summary, now
		// Acknowledged alerts don't repeat; getting worse notifies and
		// clears the acknowledgement.
		repeat := a.AckedAt == nil && c.rule.RepeatMinutes > 0 && now.Sub(a.LastNotifiedAt) >= time.Duration(c.rule.RepeatMinutes)*time.Minute
		if escalated && a.AckedAt != nil {
			s.logEvent(ctx, "alert.unacknowledged", a.DeviceID, a.DeploymentID, "acknowledgement cleared: "+a.RuleName+" got worse")
			a.AckedBy, a.AckedAt, a.AckNote = "", nil, ""
		}
		if escalated || repeat {
			if err := s.fire(ctx, c.rule, a, c.device, now); err != nil {
				return err
			}
		} else if err := s.store.PutAlert(ctx, a); err != nil {
			return err
		}
	}

	// Devices out of service (spare, in repair, retired or removed): their
	// alerts close without a "resolved" notification, which would be untrue.
	var outOfService map[string]bool
	if devices != nil {
		outOfService = map[string]bool{}
		inFleet := map[string]bool{}
		for _, d := range devices {
			inFleet[d.ID] = true
			if !d.Alerting() {
				outOfService[d.ID] = true
			}
		}
		for _, a := range active {
			if a.DeviceID != "" && !inFleet[a.DeviceID] {
				outOfService[a.DeviceID] = true
			}
		}
	}

	// Resolve alerts whose condition has cleared.
	for _, a := range active {
		if _, still := want[a.Key]; still {
			continue
		}
		if held[a.DeviceID] && a.DeploymentID == "" {
			continue // in maintenance: neither repeats nor resolves until it ends
		}
		if a.Condition == api.ConditionUpdateFailed && now.Sub(a.UpdatedAt) < eventAlertWindow {
			continue // event-based: stays open for the folding window
		}
		r := ruleByID[a.RuleID]
		a.Status, a.UpdatedAt = api.AlertResolved, now
		a.ResolvedAt = &now
		if err := s.store.PutAlert(ctx, a); err != nil {
			return err
		}
		if outOfService[a.DeviceID] && a.DeploymentID == "" {
			s.logEvent(ctx, "alert.closed", a.DeviceID, "", "closed (device out of service): "+a.RuleName+" — "+a.Summary)
			continue
		}
		if r != nil && r.Enabled && r.NotifyResolved && a.Condition != api.ConditionUpdateFailed {
			s.logEvent(ctx, "alert.resolved", a.DeviceID, a.DeploymentID, "resolved: "+a.RuleName+" — "+a.Summary)
			s.dispatch(ctx, r, a, nil)
		}
	}
	if err := s.pruneMaintenance(ctx, now); err != nil {
		return err
	}
	return s.store.PruneAlerts(ctx, now.Add(-30*24*time.Hour))
}

// alertOnEvent handles event-based conditions. Failures within one rollout
// fold into a single alert so a bad release doesn't page once per device.
func (s *Server) alertOnEvent(ctx context.Context, typ, deviceID, deploymentID, msg string) {
	if typ != "update.failed" && typ != "update.rolled_back" {
		return
	}
	rules, err := s.store.ListAlertRules(ctx)
	if err != nil {
		s.log.Error("alert rules", "err", err)
		return
	}
	var dev *api.Device
	if deviceID != "" {
		dev, _ = s.store.GetDevice(ctx, deviceID)
	}
	now := s.now()
	if dev != nil && !dev.Alerting() {
		return
	}
	if dev != nil {
		mn, err := s.activeMaintenance(ctx, now)
		if err != nil {
			s.log.Error("maintenance", "err", err)
		} else if mn.holdsAlerts(dev) {
			return
		}
	}
	for _, r := range rules {
		if !r.Enabled || r.Condition != api.ConditionUpdateFailed || !scopeEmpty(r.ClientScope) || (dev != nil && !rollout.Matches(r.Selector, dev)) {
			continue
		}
		key := r.ID + "/" + deploymentID
		var existing *api.Alert
		active, _ := s.store.ListAlerts(ctx, true, 10000)
		for _, a := range active {
			if a.Key == key {
				existing = a
			}
		}
		if existing == nil {
			a := &api.Alert{ID: newID("alr"), RuleID: r.ID, RuleName: r.Name, Condition: r.Condition, Key: key,
				Status: api.AlertFiring, Severity: api.HealthWarning, Summary: msg, DeploymentID: deploymentID,
				StartedAt: now, UpdatedAt: now, Count: 1}
			if dev != nil {
				a.DeviceID, a.DeviceName = dev.ID, dev.Name
			}
			if err := s.fire(ctx, r, a, dev, now); err != nil {
				s.log.Error("fire alert", "err", err)
			}
			continue
		}
		existing.Count++
		existing.Summary = fmt.Sprintf("%d failed updates in this rollout; latest: %s", existing.Count, msg)
		existing.UpdatedAt = now
		if existing.AckedAt == nil && r.RepeatMinutes > 0 && now.Sub(existing.LastNotifiedAt) >= time.Duration(r.RepeatMinutes)*time.Minute {
			if err := s.fire(ctx, r, existing, dev, now); err != nil {
				s.log.Error("fire alert", "err", err)
			}
			continue
		}
		if err := s.store.PutAlert(ctx, existing); err != nil {
			s.log.Error("store alert", "err", err)
		}
	}
}

func (s *Server) fire(ctx context.Context, r *api.AlertRule, a *api.Alert, dev *api.Device, now time.Time) error {
	a.LastNotifiedAt = now
	if err := s.store.PutAlert(ctx, a); err != nil {
		return err
	}
	s.logEvent(ctx, "alert."+a.Severity, a.DeviceID, a.DeploymentID, a.RuleName+": "+a.Summary)
	s.dispatch(ctx, r, a, dev)
	return nil
}

// dispatch queues a notification on each of the rule's channels.
func (s *Server) dispatch(ctx context.Context, r *api.AlertRule, a *api.Alert, dev *api.Device) {
	n := api.AlertNotification{Version: "1", AlertID: a.ID, Status: a.Status, Severity: a.Severity, Rule: r.Name,
		Condition: r.Condition, Summary: a.Summary, DeviceID: a.DeviceID, DeviceName: a.DeviceName,
		DeploymentID: a.DeploymentID, StartedAt: a.StartedAt, ResolvedAt: a.ResolvedAt, Count: a.Count}
	if dev == nil && a.DeviceID != "" {
		dev, _ = s.store.GetDevice(ctx, a.DeviceID)
	}
	if dev != nil {
		n.DeviceLabels = dev.Labels
	}
	if base := s.cfg.Android.PublicURL; base != "" {
		base = strings.TrimRight(base, "/")
		switch {
		case a.DeviceID != "":
			n.URL = base + "/#/devices/" + a.DeviceID
		case a.DeploymentID != "":
			n.URL = base + "/#/deployments/" + a.DeploymentID
		}
	}
	for _, id := range r.Channels {
		ch, err := s.store.GetChannel(ctx, id)
		if err != nil {
			s.log.Warn("alert rule refers to a missing channel", "rule", r.Name, "channel", id)
			continue
		}
		s.enqueueNotification(ch, n)
	}
}

// ruleCovers reports whether a rule applies to a device: its selector, and
// for a client rule also the client's scope.
func ruleCovers(r *api.AlertRule, d *api.Device) bool {
	return rollout.Matches(r.Selector, d) && (scopeEmpty(r.ClientScope) || rollout.Matches(r.ClientScope, d))
}

// sameScope compares two selectors (labels and channel).
func sameScope(a, b api.Selector) bool {
	if a.Channel != b.Channel || len(a.Labels) != len(b.Labels) {
		return false
	}
	for k, v := range a.Labels {
		if b.Labels[k] != v {
			return false
		}
	}
	return true
}

// alertAccess decides who sees and manages rules and channels. Admins
// manage everything. A client object (non-empty scope) is visible to anyone
// with a grant on exactly that scope and editable by its operators. Vendor
// rules are visible to fleet-wide staff; vendor channels only to admins.
func alertAccess(p *principal, sc api.Selector, channel bool) (see, edit bool) {
	if p.fleetRole() == api.RoleAdmin {
		return true, true
	}
	if scopeEmpty(sc) {
		return !channel && p.fleetRole() != "", false
	}
	for _, g := range p.grants {
		if sameScope(g.Scope, sc) {
			see = true
			edit = edit || api.RoleRank(g.Role) >= api.RoleRank(api.RoleOperator)
		}
	}
	return see, edit
}

// pickClientScope resolves which client a new rule or channel belongs to.
// Admins may leave it empty (a vendor object). Others must name one where
// they are operators, or have exactly one such scope.
func pickClientScope(p *principal, sc api.Selector, channel bool) (api.Selector, error) {
	if !scopeEmpty(sc) || p.fleetRole() == api.RoleAdmin {
		if _, edit := alertAccess(p, sc, channel); !edit {
			return sc, errors.New("you can only manage alerts for clients where you are an operator")
		}
		return sc, nil
	}
	var cands []api.Selector
	for _, g := range p.grants {
		if !scopeEmpty(g.Scope) && api.RoleRank(g.Role) >= api.RoleRank(api.RoleOperator) {
			cands = append(cands, g.Scope)
		}
	}
	switch len(cands) {
	case 0:
		return sc, errors.New("only admins manage fleet-wide alerts; client alerts need the operator role for a client")
	case 1:
		return cands[0], nil
	}
	return sc, errors.New("say which client this is for (client_scope)")
}

func roundDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- admin API: channels ----

const redacted = "********"

// redactChannel hides secrets in API responses.
func redactChannel(c api.NotificationChannel) api.NotificationChannel {
	if c.Webhook != nil {
		w := *c.Webhook
		if w.Secret != "" {
			w.Secret = redacted
		}
		if len(w.Headers) > 0 {
			h := map[string]string{}
			for k, v := range w.Headers {
				lk := strings.ToLower(k)
				if strings.Contains(lk, "auth") || strings.Contains(lk, "token") || strings.Contains(lk, "key") || strings.Contains(lk, "secret") {
					v = redacted
				}
				h[k] = v
			}
			w.Headers = h
		}
		c.Webhook = &w
	}
	if c.Slack != nil {
		sl := *c.Slack
		if u, err := url.Parse(sl.WebhookURL); err == nil && u.Host != "" {
			sl.WebhookURL = u.Scheme + "://" + u.Host + "/" + redacted
		}
		c.Slack = &sl
	}
	if c.Email != nil {
		e := *c.Email
		if e.Password != "" {
			e.Password = redacted
		}
		c.Email = &e
	}
	return c
}

// keepSecrets restores secrets the client sent back redacted.
func keepSecrets(c, old *api.NotificationChannel) {
	if c.Webhook != nil && old.Webhook != nil {
		if c.Webhook.Secret == redacted {
			c.Webhook.Secret = old.Webhook.Secret
		}
		for k, v := range c.Webhook.Headers {
			if v == redacted {
				c.Webhook.Headers[k] = old.Webhook.Headers[k]
			}
		}
	}
	if c.Slack != nil && old.Slack != nil && strings.HasSuffix(c.Slack.WebhookURL, "/"+redacted) {
		c.Slack.WebhookURL = old.Slack.WebhookURL
	}
	if c.Email != nil && old.Email != nil && c.Email.Password == redacted {
		c.Email.Password = old.Email.Password
	}
}

func validateChannel(c *api.NotificationChannel) error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return errors.New("name is required")
	}
	httpURL := func(s string) error {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("a valid http(s) URL is required")
		}
		return nil
	}
	switch c.Type {
	case "webhook":
		if c.Webhook == nil {
			return errors.New("webhook settings are required")
		}
		if err := httpURL(c.Webhook.URL); err != nil {
			return err
		}
		if c.Webhook.BodyTemplate != "" {
			if _, err := renderTemplate(c.Webhook.BodyTemplate, sampleNotification()); err != nil {
				return err
			}
		}
		c.Slack, c.Email = nil, nil
	case "slack":
		if c.Slack == nil {
			return errors.New("slack settings are required")
		}
		if err := httpURL(c.Slack.WebhookURL); err != nil {
			return err
		}
		c.Webhook, c.Email = nil, nil
	case "email":
		e := c.Email
		if e == nil || len(e.To) == 0 {
			return errors.New("email needs at least one recipient")
		}
		if e.Host != "" { // its own server; otherwise the server's relay sends it
			if _, err := mail.ParseAddress(e.From); err != nil {
				return errors.New("invalid from address")
			}
		}
		for _, to := range e.To {
			if _, err := mail.ParseAddress(to); err != nil {
				return fmt.Errorf("invalid recipient %q", to)
			}
		}
		c.Webhook, c.Slack = nil, nil
	default:
		return errors.New("type must be webhook, slack or email")
	}
	return nil
}

func sampleNotification() api.AlertNotification {
	return api.AlertNotification{Version: "1", AlertID: "alr_test", Status: "test", Severity: api.HealthWarning,
		Rule: "Test notification", Condition: api.ConditionHealth, Summary: "This is a test notification from Armada.",
		DeviceID: "dev_example", DeviceName: "example-device", DeviceLabels: map[string]string{"site": "example"},
		StartedAt: time.Now().UTC(), Count: 1}
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	chs, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := make([]api.NotificationChannel, 0, len(chs))
	for _, c := range chs {
		if see, _ := alertAccess(p, c.ClientScope, true); see {
			out = append(out, redactChannel(*c))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSaveChannel(w http.ResponseWriter, r *http.Request) {
	var c api.NotificationChannel
	if !readJSON(w, r, &c) {
		return
	}
	now := s.now()
	p := principalFrom(r.Context())
	status := http.StatusCreated
	if id := r.PathValue("id"); id != "" {
		old, err := s.store.GetChannel(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		} else if err != nil {
			s.internalError(w, err)
			return
		}
		if see, edit := alertAccess(p, old.ClientScope, true); !see {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		} else if !edit {
			writeError(w, http.StatusForbidden, "you can't change this channel")
			return
		}
		c.ID, c.CreatedAt, c.ClientScope, status = old.ID, old.CreatedAt, old.ClientScope, http.StatusOK
		keepSecrets(&c, old)
	} else {
		sc, err := pickClientScope(p, c.ClientScope, true)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		c.ID, c.CreatedAt, c.ClientScope = newID("chn"), now, sc
	}
	if err := validateChannel(&c); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.Type == "email" && c.Email.Host == "" && s.cfg.SMTPRelay.Host == "" {
		writeError(w, http.StatusBadRequest, "set the email server, or configure the server's SMTP relay (-smtp-host)")
		return
	}
	if err := s.validateClientChannel(&c); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c.UpdatedAt = now
	if err := s.store.PutChannel(r.Context(), &c); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, status, redactChannel(c))
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if !s.canEditChannel(w, r) {
		return
	}
	if err := s.store.DeleteChannel(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel sends a sample notification right away and reports the outcome.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	if !s.canEditChannel(w, r) {
		return
	}
	c, err := s.store.GetChannel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.sendNotification(ctx, c, sampleNotification()); err != nil {
		writeError(w, http.StatusBadGateway, "delivery failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// ---- admin API: rules and alerts ----

func (s *Server) handleListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListAlertRules(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.AlertRule{}
	for _, rl := range rules {
		if see, _ := alertAccess(p, rl.ClientScope, false); see {
			out = append(out, rl)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSaveAlertRule(w http.ResponseWriter, r *http.Request) {
	var rule api.AlertRule
	if !readJSON(w, r, &rule) {
		return
	}
	ctx := r.Context()
	now := s.now()
	p := principalFrom(ctx)
	status := http.StatusCreated
	if id := r.PathValue("id"); id != "" {
		old, err := s.store.GetAlertRule(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "rule not found")
			return
		} else if err != nil {
			s.internalError(w, err)
			return
		}
		if see, edit := alertAccess(p, old.ClientScope, false); !see {
			writeError(w, http.StatusNotFound, "rule not found")
			return
		} else if !edit {
			writeError(w, http.StatusForbidden, "you can't change this rule")
			return
		}
		rule.ID, rule.CreatedAt, rule.ClientScope, rule.CreatedBy, status = old.ID, old.CreatedAt, old.ClientScope, old.CreatedBy, http.StatusOK
	} else {
		sc, err := pickClientScope(p, rule.ClientScope, false)
		if err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		rule.ID, rule.CreatedAt, rule.ClientScope, rule.CreatedBy = newID("rul"), now, sc, p.name
	}
	if !scopeEmpty(rule.ClientScope) && rule.Condition != api.ConditionDeviceOffline && rule.Condition != api.ConditionHealth {
		writeError(w, http.StatusBadRequest, "client alert rules can watch device_offline or health; update failures and paused rollouts are vendor-wide")
		return
	}
	rule.Name = strings.TrimSpace(rule.Name)
	switch {
	case rule.Name == "":
		writeError(w, http.StatusBadRequest, "name is required")
		return
	case rule.Condition != api.ConditionDeviceOffline && rule.Condition != api.ConditionHealth &&
		rule.Condition != api.ConditionUpdateFailed && rule.Condition != api.ConditionDeploymentPaused:
		writeError(w, http.StatusBadRequest, "condition must be device_offline, health, update_failed or deployment_paused")
		return
	case rule.MinSeverity != "" && rule.MinSeverity != api.HealthWarning && rule.MinSeverity != api.HealthCritical:
		writeError(w, http.StatusBadRequest, "min_severity must be warning or critical")
		return
	case rule.OfflineMinutes < 0 || rule.RepeatMinutes < 0:
		writeError(w, http.StatusBadRequest, "minutes must not be negative")
		return
	}
	if err := validLabels(rule.Selector.Labels); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, id := range rule.Channels {
		ch, err := s.store.GetChannel(ctx, id)
		if err != nil {
			writeError(w, http.StatusBadRequest, "unknown channel "+id)
			return
		}
		// A client's alerts only go to that client's channels, and vendor
		// alerts only to vendor channels: no client sees another's devices.
		if !sameScope(ch.ClientScope, rule.ClientScope) {
			writeError(w, http.StatusBadRequest, "channel "+ch.Name+" belongs to a different client")
			return
		}
	}
	if rule.Channels == nil {
		rule.Channels = []string{}
	}
	rule.UpdatedAt = now
	if err := s.store.PutAlertRule(ctx, &rule); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, status, rule)
}

func (s *Server) handleDeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	old, err := s.store.GetAlertRule(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if see, edit := alertAccess(principalFrom(r.Context()), old.ClientScope, false); !see {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	} else if !edit {
		writeError(w, http.StatusForbidden, "you can't delete this rule")
		return
	}
	if err := s.store.DeleteAlertRule(r.Context(), old.ID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	alerts, err := s.store.ListAlerts(r.Context(), r.URL.Query().Get("status") == api.AlertFiring, 500)
	if err != nil {
		s.internalError(w, err)
		return
	}
	alerts = s.visibleAlerts(r.Context(), principalFrom(r.Context()), alerts)
	if mn, err := s.activeMaintenance(r.Context(), s.now()); err == nil && !mn.empty() {
		devs := map[string]*api.Device{}
		for _, a := range alerts {
			if a.Status != api.AlertFiring || a.DeviceID == "" {
				continue
			}
			d, ok := devs[a.DeviceID]
			if !ok {
				d, _ = s.store.GetDevice(r.Context(), a.DeviceID)
				devs[a.DeviceID] = d
			}
			a.InMaintenance = d != nil && mn.holdsAlerts(d)
		}
	}
	writeJSON(w, http.StatusOK, nonNil(alerts))
}

// canEditChannel checks the caller may change the channel in the path.
func (s *Server) canEditChannel(w http.ResponseWriter, r *http.Request) bool {
	c, err := s.store.GetChannel(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return false
	} else if err != nil {
		s.internalError(w, err)
		return false
	}
	see, edit := alertAccess(principalFrom(r.Context()), c.ClientScope, true)
	if !see {
		writeError(w, http.StatusNotFound, "channel not found")
		return false
	}
	if !edit {
		writeError(w, http.StatusForbidden, "you can't change this channel")
		return false
	}
	return true
}

// validateClientChannel adds the limits on channels clients configure: the
// server makes these requests on their behalf.
func (s *Server) validateClientChannel(c *api.NotificationChannel) error {
	if scopeEmpty(c.ClientScope) {
		return nil
	}
	httpsOnly := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return errors.New("client webhooks must use an https:// URL")
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && !publicIP(ip) && !s.allowPrivateClientTargets {
			return errNonPublic
		}
		return nil
	}
	switch c.Type {
	case "webhook":
		if err := httpsOnly(c.Webhook.URL); err != nil {
			return err
		}
		if c.Webhook.BodyTemplate != "" {
			return errors.New("client webhooks send the standard JSON notification; custom body templates are vendor-only")
		}
		if m := strings.ToUpper(c.Webhook.Method); m != "" && m != http.MethodPost && m != http.MethodPut {
			return errors.New("client webhooks use POST or PUT")
		}
	case "slack":
		if err := httpsOnly(c.Slack.WebhookURL); err != nil {
			return err
		}
	case "email":
		if s.cfg.SMTPRelay.Host == "" {
			return errors.New("email alerts for clients need the server's SMTP relay (-smtp-host); ask your vendor")
		}
		if len(c.Email.To) > 20 {
			return errors.New("at most 20 recipients")
		}
		// Clients choose recipients only; the relay sends.
		c.Email = &api.EmailConfig{To: c.Email.To}
	}
	return nil
}
