package server

import (
	"context"
	"errors"
	"fmt"
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
				if !rollout.Matches(r.Selector, d) || d.LastSeenAt.IsZero() {
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
				if !rollout.Matches(r.Selector, d) {
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
		repeat := c.rule.RepeatMinutes > 0 && now.Sub(a.LastNotifiedAt) >= time.Duration(c.rule.RepeatMinutes)*time.Minute
		if escalated || repeat {
			if err := s.fire(ctx, c.rule, a, c.device, now); err != nil {
				return err
			}
		} else if err := s.store.PutAlert(ctx, a); err != nil {
			return err
		}
	}

	// Resolve alerts whose condition has cleared.
	for _, a := range active {
		if _, still := want[a.Key]; still {
			continue
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
		if r != nil && r.Enabled && r.NotifyResolved && a.Condition != api.ConditionUpdateFailed {
			s.logEvent(ctx, "alert.resolved", a.DeviceID, a.DeploymentID, "resolved: "+a.RuleName+" — "+a.Summary)
			s.dispatch(ctx, r, a, nil)
		}
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
	for _, r := range rules {
		if !r.Enabled || r.Condition != api.ConditionUpdateFailed || (dev != nil && !rollout.Matches(r.Selector, dev)) {
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
		if r.RepeatMinutes > 0 && now.Sub(existing.LastNotifiedAt) >= time.Duration(r.RepeatMinutes)*time.Minute {
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
		if e == nil || e.Host == "" || len(e.To) == 0 {
			return errors.New("email needs host, from and at least one recipient")
		}
		if _, err := mail.ParseAddress(e.From); err != nil {
			return errors.New("invalid from address")
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
	out := make([]api.NotificationChannel, 0, len(chs))
	for _, c := range chs {
		out = append(out, redactChannel(*c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSaveChannel(w http.ResponseWriter, r *http.Request) {
	var c api.NotificationChannel
	if !readJSON(w, r, &c) {
		return
	}
	now := s.now()
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
		c.ID, c.CreatedAt, status = old.ID, old.CreatedAt, http.StatusOK
		keepSecrets(&c, old)
	} else {
		c.ID, c.CreatedAt = newID("chn"), now
	}
	if err := validateChannel(&c); err != nil {
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
	c, err := s.store.GetChannel(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel not found")
		return
	} else if err != nil {
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
	writeJSON(w, http.StatusOK, nonNil(rules))
}

func (s *Server) handleSaveAlertRule(w http.ResponseWriter, r *http.Request) {
	var rule api.AlertRule
	if !readJSON(w, r, &rule) {
		return
	}
	ctx := r.Context()
	now := s.now()
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
		rule.ID, rule.CreatedAt, status = old.ID, old.CreatedAt, http.StatusOK
	} else {
		rule.ID, rule.CreatedAt = newID("rul"), now
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
		if _, err := s.store.GetChannel(ctx, id); err != nil {
			writeError(w, http.StatusBadRequest, "unknown channel "+id)
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
	if err := s.store.DeleteAlertRule(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
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
	writeJSON(w, http.StatusOK, nonNil(alerts))
}
