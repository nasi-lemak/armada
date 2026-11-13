package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Renewal policies make certificate renewal hands-off up to the signature:
// RenewDays before a certificate expires they start its renewal (the
// device makes a new key and CSR), email the CSRs in one batch to whoever
// signs them, and remind them every ReminderDays while certificates are
// still to come back. Certificates that come back are installed as usual.

const (
	certPolicyInterval = 15 * time.Minute
	// A renewal that failed or was cancelled is retried after this long.
	certPolicyRetry = 7 * 24 * time.Hour
)

type certPolicyResult struct {
	Started  int `json:"started"`
	Emailed  int `json:"emailed"`
	Reminded int `json:"reminded"`
}

// certPolicyTick runs the enabled renewal policies every certPolicyInterval.
func (s *Server) certPolicyTick(ctx context.Context, now time.Time) error {
	s.certPolicyMu.Lock()
	due := now.Sub(s.certPolicyLastRun) >= certPolicyInterval
	if due {
		s.certPolicyLastRun = now
	}
	s.certPolicyMu.Unlock()
	if !due {
		return nil
	}
	pols, err := s.store.ListCertPolicies(ctx)
	if err != nil {
		return err
	}
	for _, p := range pols {
		if !p.Enabled {
			continue
		}
		if _, err := s.runCertPolicy(ctx, p); err != nil {
			s.log.Warn("certificate policy", "policy", p.Name, "err", err)
		}
	}
	return nil
}

// runCertPolicy does one pass of a policy. Runs are serialised so a "run
// now" and the engine never start the same renewal twice.
func (s *Server) runCertPolicy(ctx context.Context, pol *api.CertPolicy) (certPolicyResult, error) {
	s.certPolicyRunMu.Lock()
	defer s.certPolicyRunMu.Unlock()
	var res certPolicyResult
	now := s.now()
	// It acts as an operator on exactly its client's devices.
	ctx = context.WithValue(ctx, principalKey{}, &principal{name: "renewal policy " + pol.Name,
		grants: []api.Grant{{Role: api.RoleOperator, Scope: pol.ClientScope}}})
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return res, err
	}
	covered := map[string]*api.Device{}
	for _, d := range devs {
		if d.Lifecycle == api.LifecycleActive && rollout.Matches(pol.ClientScope, d) && rollout.Matches(pol.Selector, d) {
			covered[d.ID] = d
		}
	}
	within := pol.RenewDays
	if within <= 0 {
		within = 30
	}
	all, err := s.store.ListDeviceCerts(ctx, "")
	if err != nil {
		return res, err
	}
	var wake []string
	for _, c := range all {
		d := covered[c.DeviceID]
		if d == nil || c.Installed == nil || (len(pol.Targets) > 0 && !containsStr(pol.Targets, c.Target)) {
			continue
		}
		if left := daysLeft(c.Installed, now); left == nil || *left > within {
			continue
		}
		if r := c.Renewal; r != nil {
			if renewalActive(r) {
				continue
			}
			// Don't keep retrying what failed or someone cancelled: try again weekly.
			if (r.Status == api.RenewalFailed || r.Status == api.RenewalCancelled) && now.Sub(r.UpdatedAt) < certPolicyRetry {
				continue
			}
		}
		if err := s.startRenewal(ctx, c, d, api.RenewCertsRequest{KeyType: pol.KeyType}, pol); err != nil {
			var bad renewalSpecError
			if errors.As(err, &bad) {
				continue
			}
			return res, err
		}
		res.Started++
		wake = append(wake, d.ID)
	}
	s.wakeIDs(wake)

	// Send the CSRs that are ready, and remind about old ones.
	if len(pol.Recipients) > 0 && s.cfg.SMTPRelay.Host != "" {
		if all, err = s.store.ListDeviceCerts(ctx, ""); err != nil {
			return res, err
		}
		var fresh, stale []*api.DeviceCert
		for _, c := range all {
			r := c.Renewal
			d := covered[c.DeviceID]
			if d == nil || r == nil || r.PolicyID != pol.ID || r.Status != api.RenewalCSRReady {
				continue
			}
			c.DeviceName = d.Name
			switch {
			case r.EmailedAt == nil:
				fresh = append(fresh, c)
			case pol.ReminderDays > 0:
				last := *r.EmailedAt
				if r.RemindedAt != nil && r.RemindedAt.After(last) {
					last = *r.RemindedAt
				}
				if now.Sub(last) >= time.Duration(pol.ReminderDays)*24*time.Hour {
					stale = append(stale, c)
				}
			}
		}
		if len(fresh) > 0 {
			if err := s.emailCSRs(ctx, fresh, pol.Recipients, pol.Note, false); err != nil {
				s.logEvent(ctx, "cert.email_failed", "", "", fmt.Sprintf("renewal policy %q couldn't email %d CSRs: %v", pol.Name, len(fresh), err))
			} else {
				res.Emailed = len(fresh)
			}
		}
		if len(stale) > 0 {
			if err := s.emailCSRs(ctx, stale, pol.Recipients, pol.Note, true); err != nil {
				s.logEvent(ctx, "cert.email_failed", "", "", fmt.Sprintf("renewal policy %q couldn't send a reminder: %v", pol.Name, err))
			} else {
				res.Reminded = len(stale)
			}
		}
	}
	pol.LastRunAt = &now
	if err := s.store.PutCertPolicy(ctx, pol); err != nil {
		return res, err
	}
	return res, nil
}

// ---- handlers ----

func (s *Server) handleListCertPolicies(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListCertPolicies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.CertPolicy{}
	for _, pol := range all {
		if see, _ := alertAccess(p, pol.ClientScope, false); see {
			out = append(out, pol)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// checkCertPolicy validates a policy's settings.
func (s *Server) checkCertPolicy(pol *api.CertPolicy) error {
	pol.Name, pol.Note = strings.TrimSpace(pol.Name), strings.TrimSpace(pol.Note)
	if pol.RenewDays == 0 {
		pol.RenewDays = 30
	}
	switch {
	case pol.RenewDays < 1 || pol.RenewDays > 365:
		return errors.New("renew_days must be between 1 and 365")
	case pol.ReminderDays < 0 || pol.ReminderDays > 90:
		return errors.New("reminder_days must be between 0 (no reminders) and 90")
	case pol.KeyType != "" && !certs.ValidKeyType(pol.KeyType):
		return errors.New("key_type must be one of " + strings.Join(certs.KeyTypes, ", "))
	case len(pol.Recipients) > 20:
		return errors.New("at most 20 recipients")
	case len(pol.Note) > 1000 || len(pol.Name) > 100:
		return errors.New("name is limited to 100 characters and note to 1000")
	case len(pol.Recipients) > 0 && s.cfg.SMTPRelay.Host == "":
		return errors.New("emailing CSRs needs the server's SMTP relay (-smtp-host); leave recipients empty to collect CSRs on the Certificates page")
	}
	for _, t := range pol.Targets {
		if !validTarget(t) {
			return fmt.Errorf("invalid certificate name %q", t)
		}
	}
	for i, to := range pol.Recipients {
		a, err := mail.ParseAddress(strings.TrimSpace(to))
		if err != nil {
			return fmt.Errorf("invalid recipient %q", to)
		}
		pol.Recipients[i] = a.Address
	}
	return validLabels(pol.Selector.Labels)
}

func (s *Server) handleCreateCertPolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var pol api.CertPolicy
	if !readJSON(w, r, &pol) {
		return
	}
	if err := s.checkCertPolicy(&pol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sc, err := pickClientScope(p, pol.ClientScope, false, "certificate renewal")
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	now := s.now()
	pol.ClientScope, pol.ID, pol.Enabled, pol.CreatedBy, pol.CreatedAt, pol.UpdatedAt, pol.LastRunAt = sc, newID("cpl"), true, p.name, now, now, nil
	if pol.Name == "" {
		pol.Name = "Renew " + strings.TrimSpace(scopeText(sc)+" certificates")
		if scopeEmpty(sc) {
			pol.Name = "Renew certificates"
		}
	}
	if err := s.store.PutCertPolicy(ctx, &pol); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "cert.policy_created", "", "", fmt.Sprintf("renewal policy %q: %d days before expiry%s", pol.Name, pol.RenewDays, policyMail(&pol)))
	writeJSON(w, http.StatusCreated, pol)
}

func policyMail(pol *api.CertPolicy) string {
	if len(pol.Recipients) == 0 {
		return ", CSRs collected on the Certificates page"
	}
	return ", CSRs emailed to " + strings.Join(pol.Recipients, ", ")
}

func (s *Server) loadCertPolicy(w http.ResponseWriter, r *http.Request) (*api.CertPolicy, bool) {
	pol, err := s.store.GetCertPolicy(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "renewal policy not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	see, edit := alertAccess(principalFrom(r.Context()), pol.ClientScope, false)
	if !see {
		writeError(w, http.StatusNotFound, "renewal policy not found")
		return nil, false
	}
	if !edit {
		writeError(w, http.StatusForbidden, "you can't change this renewal policy")
		return nil, false
	}
	return pol, true
}

func (s *Server) handleUpdateCertPolicy(w http.ResponseWriter, r *http.Request) {
	old, ok := s.loadCertPolicy(w, r)
	if !ok {
		return
	}
	var pol api.CertPolicy
	if !readJSON(w, r, &pol) {
		return
	}
	if err := s.checkCertPolicy(&pol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pol.Name == "" {
		pol.Name = old.Name
	}
	// Which client it's for doesn't change.
	pol.ID, pol.ClientScope, pol.CreatedBy, pol.CreatedAt, pol.LastRunAt, pol.UpdatedAt = old.ID, old.ClientScope, old.CreatedBy, old.CreatedAt, old.LastRunAt, s.now()
	if err := s.store.PutCertPolicy(r.Context(), &pol); err != nil {
		s.internalError(w, err)
		return
	}
	state := "updated"
	if old.Enabled != pol.Enabled {
		state = map[bool]string{true: "enabled", false: "disabled"}[pol.Enabled]
	}
	s.logEvent(r.Context(), "cert.policy_updated", "", "", fmt.Sprintf("renewal policy %q %s", pol.Name, state))
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handleDeleteCertPolicy(w http.ResponseWriter, r *http.Request) {
	pol, ok := s.loadCertPolicy(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteCertPolicy(r.Context(), pol.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "cert.policy_deleted", "", "", fmt.Sprintf("renewal policy %q deleted (renewals it started carry on)", pol.Name))
	w.WriteHeader(http.StatusNoContent)
}

// handleRunCertPolicy checks a policy's certificates now instead of waiting.
func (s *Server) handleRunCertPolicy(w http.ResponseWriter, r *http.Request) {
	pol, ok := s.loadCertPolicy(w, r)
	if !ok {
		return
	}
	res, err := s.runCertPolicy(context.WithoutCancel(r.Context()), pol)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
