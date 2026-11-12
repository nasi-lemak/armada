package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// recordHealth stores a device's check results and logs status changes.
func (s *Server) recordHealth(ctx context.Context, dev *api.Device, checks []api.CheckResult) error {
	for i := range checks {
		checks[i].Name = truncate(checks[i].Name, 128)
		checks[i].Message = truncate(checks[i].Message, 1024)
		if checks[i].Status == "" {
			checks[i].Status = api.HealthUnknown
		}
	}
	if len(checks) > 500 {
		checks = checks[:500]
	}
	prev := map[string]api.CheckResult{}
	for _, c := range dev.Checks {
		prev[c.Name] = c
	}
	for _, c := range checks {
		old, existed := prev[c.Name]
		oldStatus := api.HealthOK
		if existed {
			oldStatus = old.Status
		}
		if oldStatus == c.Status || (!existed && c.Status == api.HealthOK) {
			continue
		}
		msg := fmt.Sprintf("%s: %s %s → %s", dev.Name, c.Name, oldStatus, c.Status)
		if c.Message != "" {
			msg += " (" + c.Message + ")"
		}
		s.event(ctx, "health."+c.Status, dev.ID, "", msg)
	}
	if err := s.store.SetDeviceHealth(ctx, dev.ID, checks); err != nil {
		return err
	}
	dev.Checks, dev.Health = checks, api.WorstHealth(checks)
	return nil
}

// monitorsFor returns the enabled monitors whose selector matches the device.
func (s *Server) monitorsFor(ctx context.Context, dev *api.Device) ([]api.MonitorCheck, error) {
	ms, err := s.store.ListMonitors(ctx)
	if err != nil {
		return nil, err
	}
	var out []api.MonitorCheck
	for _, m := range ms {
		if m.Enabled && rollout.Matches(m.Selector, dev) {
			out = append(out, api.MonitorCheck{Name: m.Name, Check: m.Check})
		}
	}
	return out, nil
}

// ---- admin API ----

func (s *Server) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListMonitors(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(ms))
}

func validateCheck(c *api.CheckSpec) error {
	switch c.Type {
	case "systemd", "process":
		if strings.TrimSpace(c.Target) == "" {
			return errors.New("target (unit or process name) is required")
		}
	case "http":
		u, err := url.Parse(c.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("target must be an http(s) URL")
		}
	case "tcp":
		if _, _, err := splitHostPort(c.Target); err != nil {
			return errors.New("target must be host:port")
		}
	case "command":
		if len(c.Command) == 0 {
			return errors.New("command is required")
		}
	default:
		return errors.New("type must be systemd, process, http, tcp or command")
	}
	if c.IntervalSeconds != 0 && c.IntervalSeconds < 10 {
		return errors.New("interval_seconds must be at least 10")
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 300 {
		return errors.New("timeout_seconds must be between 0 and 300")
	}
	return nil
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", errors.New("missing port")
	}
	return s[:i], s[i+1:], nil
}

func (s *Server) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	var m api.Monitor
	if !readJSON(w, r, &m) {
		return
	}
	m.ID = newID("mon")
	s.saveMonitor(w, r, &m, true)
}

func (s *Server) handleUpdateMonitor(w http.ResponseWriter, r *http.Request) {
	old, err := s.store.GetMonitor(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "monitor not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	var m api.Monitor
	if !readJSON(w, r, &m) {
		return
	}
	m.ID, m.CreatedAt = old.ID, old.CreatedAt
	s.saveMonitor(w, r, &m, false)
}

func (s *Server) saveMonitor(w http.ResponseWriter, r *http.Request, m *api.Monitor, create bool) {
	// A command check runs arbitrary code as root on every matching device
	// (where the agent allows exec), so only admins may set one up or change it.
	if m.Check.Type == "command" || (!create && s.wasCommandMonitor(r, m.ID)) {
		if principalFrom(r.Context()).fleetRole() != api.RoleAdmin {
			writeError(w, http.StatusForbidden, "command checks run code as root on devices; only admins can create or change them")
			return
		}
	}
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" || len(m.Name) > 64 {
		writeError(w, http.StatusBadRequest, "name is required (max 64 characters)")
		return
	}
	if err := validateCheck(&m.Check); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validLabels(m.Selector.Labels); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	if create {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if err := s.store.UpsertMonitor(r.Context(), m); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a monitor with this name already exists")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	writeJSON(w, status, m)
}

func (s *Server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteMonitor(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "monitor not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) wasCommandMonitor(r *http.Request, id string) bool {
	old, err := s.store.GetMonitor(r.Context(), id)
	return err == nil && old.Check.Type == "command"
}
