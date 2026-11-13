package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/certs"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Certificates on devices (Mosquitto, web servers, …) are tracked from what
// agents report about their managed_certs, and renewed by CSR:
//
//  1. A renewal asks the device for a CSR. The agent makes a new key, keeps
//     it, and returns only the CSR.
//  2. Someone with the client's CA signs it: CSRs can be downloaded (zip),
//     emailed, or fetched by the client on the Certificates page.
//  3. Signed certificates are uploaded — any number, in any order — and
//     matched to waiting renewals by public key, then checked.
//  4. The device installs the certificate with its key, reloads the
//     service, checks it's served, and rolls back if not.

const (
	certReportRefresh = time.Hour
	maxCertUpload     = 1 << 20
	certCriticalDays  = 7
)

func certID(deviceID, target string) string { return deviceID + "/" + target }

func renewalActive(r *api.CertRenewal) bool {
	return r != nil && (r.Status == api.RenewalRequested || r.Status == api.RenewalCSRReady || r.Status == api.RenewalInstalling)
}

func daysLeft(info *api.CertInfo, now time.Time) *int {
	if info == nil {
		return nil
	}
	d := int(info.NotAfter.Sub(now).Hours() / 24)
	if info.NotAfter.Before(now) {
		d = -int(now.Sub(info.NotAfter).Hours()/24) - 1
	}
	return &d
}

// syncCerts records what an agent reported about its managed certificates.
func (s *Server) syncCerts(ctx context.Context, dev *api.Device, reports []api.CertReport, now time.Time) {
	existing, err := s.store.ListDeviceCerts(ctx, dev.ID)
	if err != nil {
		s.log.Error("load certificates", "device", dev.ID, "err", err)
		return
	}
	byTarget := map[string]*api.DeviceCert{}
	for _, c := range existing {
		byTarget[c.Target] = c
	}
	seen := map[string]bool{}
	for _, r := range reports {
		if !validTarget(r.Target) {
			continue
		}
		seen[r.Target] = true
		c := byTarget[r.Target]
		if c == nil {
			c = &api.DeviceCert{ID: certID(dev.ID, r.Target), DeviceID: dev.ID, Target: r.Target}
		}
		changed := c.Error != r.Error || (c.Installed == nil) != (r.Info == nil) ||
			(c.Installed != nil && r.Info != nil && (c.Installed.Fingerprint != r.Info.Fingerprint || c.Installed.KeyMatches != r.Info.KeyMatches))
		// A renewal whose certificate is now installed is done, even if the
		// action's result went missing.
		if rn := c.Renewal; rn != nil && rn.Status == api.RenewalInstalling && rn.Certificate != nil && r.Info != nil && r.Info.Fingerprint == rn.Certificate.Fingerprint {
			rn.Status, rn.InstalledAt, rn.UpdatedAt, rn.Message = api.RenewalInstalled, &now, now, ""
			changed = true
		}
		if !changed && c.DeviceName == dev.Name && now.Sub(c.ReportedAt) < certReportRefresh {
			continue
		}
		c.DeviceName, c.Installed, c.Error, c.ReportedAt = dev.Name, r.Info, r.Error, now
		if err := s.store.PutDeviceCert(ctx, c); err != nil {
			s.log.Error("store certificate", "device", dev.ID, "err", err)
		}
	}
	for target, c := range byTarget {
		if seen[target] {
			continue
		}
		// No longer in the agent's managed_certs.
		if renewalActive(c.Renewal) {
			c.Error = "no longer in the device's managed_certs"
			s.store.PutDeviceCert(ctx, c)
			continue
		}
		s.store.DeleteDeviceCert(ctx, c.ID)
	}
}

func validTarget(t string) bool {
	if t == "" || len(t) > 64 {
		return false
	}
	for _, r := range t {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// visibleCerts lists the certificate records of devices p can see (with
// the devices), filtered by a selector.
func (s *Server) visibleCerts(ctx context.Context, p *principal, sel api.Selector) ([]*api.DeviceCert, map[string]*api.Device, error) {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]*api.Device{}
	for _, d := range devs {
		if p.sees(d) && rollout.Matches(sel, d) {
			byID[d.ID] = d
		}
	}
	all, err := s.store.ListDeviceCerts(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	var out []*api.DeviceCert
	for _, c := range all {
		if d := byID[c.DeviceID]; d != nil {
			c.DeviceName = d.Name
			c.DaysLeft = daysLeft(c.Installed, now)
			out = append(out, c)
		}
	}
	return out, byID, nil
}

func selectorFromQuery(r *http.Request) api.Selector {
	q := r.URL.Query()
	sel := api.Selector{Channel: q.Get("channel"), DeviceIDs: q["device_id"]}
	for _, l := range q["label"] {
		if k, v, ok := strings.Cut(l, "="); ok {
			if sel.Labels == nil {
				sel.Labels = map[string]string{}
			}
			sel.Labels[k] = v
		}
	}
	return sel
}

func (s *Server) handleListCerts(w http.ResponseWriter, r *http.Request) {
	list, _, err := s.visibleCerts(r.Context(), principalFrom(r.Context()), selectorFromQuery(r))
	if err != nil {
		s.internalError(w, err)
		return
	}
	q := r.URL.Query()
	expiring := -1
	if v := q.Get("expiring_within"); v != "" {
		fmt.Sscan(v, &expiring)
	}
	status := q.Get("renewal")
	out := []*api.DeviceCert{}
	for _, c := range list {
		if expiring >= 0 && (c.DaysLeft == nil || *c.DaysLeft > expiring) {
			continue
		}
		if status != "" && (c.Renewal == nil || c.Renewal.Status != status) {
			continue
		}
		out = append(out, c)
	}
	// Soonest to expire first; certificates that can't be read up top.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].DaysLeft, out[j].DaysLeft
		switch {
		case a == nil || b == nil:
			return a == nil && b != nil
		case *a != *b:
			return *a < *b
		}
		return out[i].ID < out[j].ID
	})
	writeJSON(w, http.StatusOK, out)
}

// ---- renewal: CSR on the device ----

type certTemplateData struct {
	Name, ID, Hostname, IP string
	Labels                 map[string]string
}

func renderCertTemplate(v string, d *api.Device) (string, error) {
	if !strings.Contains(v, "{{") {
		return v, nil
	}
	t, err := template.New("v").Option("missingkey=error").Parse(v)
	if err != nil {
		return "", err
	}
	data := certTemplateData{Name: d.Name, ID: d.ID, Hostname: d.Info.Hostname, Labels: d.Labels}
	if len(d.Info.IPs) > 0 {
		data.IP = d.Info.IPs[0]
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// renewalSpec works out what a device's CSR asks for: the request's
// overrides (rendered for the device), otherwise the installed certificate's.
func renewalSpec(req api.RenewCertsRequest, c *api.DeviceCert, d *api.Device) (api.CertCSRSpec, error) {
	spec := api.CertCSRSpec{Target: c.Target, KeyType: req.KeyType}
	if c.Installed != nil {
		spec.Subject, spec.DNSNames, spec.IPAddresses = c.Installed.Fields, c.Installed.DNSNames, c.Installed.IPAddresses
		if spec.KeyType == "" && certs.ValidKeyType(c.Installed.KeyType) {
			spec.KeyType = c.Installed.KeyType
		}
	}
	if spec.KeyType == "" {
		spec.KeyType = "ecdsa-p256"
	}
	render := func(dst *string, v string) error {
		if v == "" {
			return nil
		}
		out, err := renderCertTemplate(v, d)
		if err != nil {
			return fmt.Errorf("template %q: %v", v, err)
		}
		*dst = out
		return nil
	}
	o := req.Subject
	for _, f := range []struct {
		dst *string
		v   string
	}{{&spec.Subject.CommonName, o.CommonName}, {&spec.Subject.Organization, o.Organization}, {&spec.Subject.OrganizationalUnit, o.OrganizationalUnit},
		{&spec.Subject.Country, o.Country}, {&spec.Subject.Province, o.Province}, {&spec.Subject.Locality, o.Locality}} {
		if err := render(f.dst, f.v); err != nil {
			return spec, err
		}
	}
	list := func(in []string) ([]string, error) {
		var out []string
		for _, v := range in {
			r, err := renderCertTemplate(v, d)
			if err != nil {
				return nil, fmt.Errorf("template %q: %v", v, err)
			}
			if r != "" {
				out = append(out, r)
			}
		}
		return out, nil
	}
	var err error
	if len(req.DNSNames) > 0 {
		if spec.DNSNames, err = list(req.DNSNames); err != nil {
			return spec, err
		}
	}
	if len(req.IPAddresses) > 0 {
		if spec.IPAddresses, err = list(req.IPAddresses); err != nil {
			return spec, err
		}
	}
	for _, ip := range spec.IPAddresses {
		if net.ParseIP(ip) == nil {
			return spec, fmt.Errorf("invalid IP address %q", ip)
		}
	}
	if spec.Subject.CommonName == "" && len(spec.DNSNames) == 0 && len(spec.IPAddresses) == 0 {
		return spec, errors.New("no certificate installed to copy from: give a common name or DNS names")
	}
	return spec, nil
}

func (s *Server) handleRenewCerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var req api.RenewCertsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.KeyType != "" && !certs.ValidKeyType(req.KeyType) {
		writeError(w, http.StatusBadRequest, "key_type must be one of "+strings.Join(certs.KeyTypes, ", "))
		return
	}
	if scopeEmpty(req.Selector) && len(req.Selector.DeviceIDs) == 0 && !req.All {
		writeError(w, http.StatusBadRequest, "say which devices (device_ids, labels or channel), or all=true")
		return
	}
	list, devs, err := s.visibleCerts(ctx, p, req.Selector)
	if err != nil {
		s.internalError(w, err)
		return
	}
	resp := api.RenewCertsResponse{Started: []api.DeviceCert{}, Skipped: []api.BulkTarget{}}
	now := s.now()
	var wake []string
	for _, c := range list {
		if len(req.Targets) > 0 && !containsStr(req.Targets, c.Target) {
			continue
		}
		d := devs[c.DeviceID]
		skip := func(reason string) {
			resp.Skipped = append(resp.Skipped, api.BulkTarget{ID: c.ID, Name: d.Name + " · " + c.Target, Reason: reason})
		}
		switch {
		case api.RoleRank(p.roleFor(d)) < api.RoleRank(api.RoleOperator):
			skip("needs the operator role on this device")
			continue
		case d.Lifecycle == api.LifecycleRetired:
			skip("the device is retired")
			continue
		case renewalActive(c.Renewal):
			skip("a renewal is already in progress (" + c.Renewal.Status + "); cancel it first")
			continue
		}
		if err := s.startRenewal(ctx, c, d, req, nil); err != nil {
			var bad renewalSpecError
			if errors.As(err, &bad) {
				skip(bad.Error())
				continue
			}
			s.internalError(w, err)
			return
		}
		c.DaysLeft = daysLeft(c.Installed, now)
		resp.Started = append(resp.Started, *c)
		wake = append(wake, d.ID)
	}
	if len(resp.Started) == 0 && len(resp.Skipped) == 0 {
		writeError(w, http.StatusNotFound, "no managed certificates on those devices (certificates are listed in the agent's managed_certs)")
		return
	}
	s.wakeIDs(wake)
	writeJSON(w, http.StatusOK, resp)
}

// renewalSpecError is a certificate a renewal can't be made for (a reason to skip it).
type renewalSpecError struct{ error }

// startRenewal asks a device for a new key and CSR. The caller has checked
// it may (role, lifecycle, no renewal in progress); the actor is whoever
// ctx names. A policy that started it is recorded.
func (s *Server) startRenewal(ctx context.Context, c *api.DeviceCert, d *api.Device, req api.RenewCertsRequest, policy *api.CertPolicy) error {
	spec, err := renewalSpec(req, c, d)
	if err != nil {
		return renewalSpecError{err}
	}
	now := s.now()
	spec.RenewalID = newID("rnw")
	args, _ := json.Marshal(spec)
	a, err := s.queueAction(ctx, d, api.CreateActionRequest{Type: api.ActionCertCSR, Args: []string{string(args)}}, "")
	if err != nil {
		return err
	}
	c.Renewal = &api.CertRenewal{ID: spec.RenewalID, Status: api.RenewalRequested, Spec: spec, RequestedBy: principalFrom(ctx).name,
		RequestedAt: now, ActionID: a.ID, UpdatedAt: now}
	why := ""
	if policy != nil {
		c.Renewal.PolicyID, c.Renewal.PolicyName = policy.ID, policy.Name
		if left := daysLeft(c.Installed, now); left != nil {
			why = fmt.Sprintf(" (expires in %d days; policy %q)", *left, policy.Name)
		}
	}
	if err := s.store.PutDeviceCert(ctx, c); err != nil {
		return err
	}
	s.logEvent(ctx, "cert.renewal_requested", d.ID, "", fmt.Sprintf("%s: new %s key and CSR requested for %s%s", d.Name, spec.KeyType, describeSpec(spec), why))
	return nil
}

func describeSpec(spec api.CertCSRSpec) string {
	names := append(append([]string{}, spec.DNSNames...), spec.IPAddresses...)
	out := spec.Target
	if spec.Subject.CommonName != "" {
		out += " (CN " + spec.Subject.CommonName
		if len(names) > 0 {
			out += "; " + strings.Join(names, ", ")
		}
		out += ")"
	} else if len(names) > 0 {
		out += " (" + strings.Join(names, ", ") + ")"
	}
	return out
}

func (s *Server) handleCancelRenewal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.store.GetDeviceCert(ctx, certID(r.PathValue("device"), r.PathValue("target")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "certificate not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	d, err := s.store.GetDevice(ctx, c.DeviceID)
	if err != nil || !principalFrom(ctx).sees(d) {
		writeError(w, http.StatusNotFound, "certificate not found")
		return
	}
	if !needDeviceRole(w, r, d, api.RoleOperator) {
		return
	}
	if c.Renewal == nil || (c.Renewal.Status != api.RenewalRequested && c.Renewal.Status != api.RenewalCSRReady) {
		writeError(w, http.StatusConflict, "no renewal waiting to be cancelled")
		return
	}
	c.Renewal.Status, c.Renewal.UpdatedAt = api.RenewalCancelled, s.now()
	if err := s.store.PutDeviceCert(ctx, c); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "cert.renewal_cancelled", d.ID, "", fmt.Sprintf("%s: %s certificate renewal cancelled", d.Name, c.Target))
	writeJSON(w, http.StatusOK, c)
}

// certActionResult follows a certificate action's result into its renewal.
func (s *Server) certActionResult(ctx context.Context, dev *api.Device, a *api.Action, res api.ActionResult) {
	if len(a.Args) != 1 {
		return
	}
	var ref struct {
		Target    string `json:"target"`
		RenewalID string `json:"renewal_id"`
	}
	if json.Unmarshal([]byte(a.Args[0]), &ref) != nil {
		return
	}
	c, err := s.store.GetDeviceCert(ctx, certID(dev.ID, ref.Target))
	if err != nil || c.Renewal == nil || c.Renewal.ID != ref.RenewalID {
		return // replaced or cancelled meanwhile
	}
	rn, now := c.Renewal, s.now()
	rn.UpdatedAt = now
	fail := func(msg string) {
		rn.Status, rn.Message = api.RenewalFailed, msg
		s.logEvent(ctx, "cert.renewal_failed", dev.ID, "", fmt.Sprintf("%s: %s certificate renewal failed: %s", dev.Name, c.Target, firstLineOf(msg)))
	}
	switch a.Type {
	case api.ActionCertCSR:
		if rn.Status != api.RenewalRequested {
			return
		}
		if res.Status != api.ActionSucceeded {
			fail("the device couldn't make the CSR: " + res.Output)
			break
		}
		csr, err := certs.ParseCSR([]byte(res.Output))
		if err != nil {
			fail("the device returned an unusable CSR: " + err.Error())
			break
		}
		rn.Status, rn.CSR, rn.PublicKey, rn.CSRAt, rn.Message = api.RenewalCSRReady, res.Output, certs.PublicKeyID(csr.PublicKey), &now, ""
		s.logEvent(ctx, "cert.csr_ready", dev.ID, "", fmt.Sprintf("%s: CSR for %s ready to be signed", dev.Name, describeSpec(rn.Spec)))
	case api.ActionCertInstall:
		if rn.Status != api.RenewalInstalling {
			return
		}
		if res.Status != api.ActionSucceeded {
			fail(res.Output)
			break
		}
		var info api.CertInfo
		if json.Unmarshal([]byte(res.Output), &info) == nil {
			c.Installed = &info
		}
		rn.Status, rn.InstalledAt, rn.Message = api.RenewalInstalled, &now, ""
		exp := ""
		if c.Installed != nil {
			exp = ", valid until " + c.Installed.NotAfter.Format("2 Jan 2006")
		}
		s.logEvent(ctx, "cert.installed", dev.ID, "", fmt.Sprintf("%s: new %s certificate installed%s", dev.Name, c.Target, exp))
	}
	if err := s.store.PutDeviceCert(ctx, c); err != nil {
		s.log.Error("store certificate", "device", dev.ID, "err", err)
	}
}

// plural writes "1 thing" or "3 things".
func plural(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return truncate(s, 200)
}

// ---- CSRs out: zip download and email ----

func (s *Server) waitingCSRs(ctx context.Context, p *principal, sel api.Selector, needOperator bool) ([]*api.DeviceCert, error) {
	list, devs, err := s.visibleCerts(ctx, p, sel)
	if err != nil {
		return nil, err
	}
	var out []*api.DeviceCert
	for _, c := range list {
		if c.Renewal == nil || c.Renewal.Status != api.RenewalCSRReady {
			continue
		}
		if needOperator && api.RoleRank(p.roleFor(devs[c.DeviceID])) < api.RoleRank(api.RoleOperator) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// csrZip packs CSRs with a manifest listing what each asks for.
func csrZip(list []*api.DeviceCert) ([]byte, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	var manifest strings.Builder
	manifest.WriteString("file,device_id,device,target,common_name,dns_names,ip_addresses,key_type\n")
	used := map[string]bool{}
	for _, c := range list {
		name := fileSafe(c.DeviceName + "_" + c.Target)
		for i := 2; used[name]; i++ {
			name = fileSafe(fmt.Sprintf("%s_%s_%d", c.DeviceName, c.Target, i))
		}
		used[name] = true
		f, err := z.Create(name + ".csr")
		if err != nil {
			return nil, err
		}
		f.Write([]byte(c.Renewal.CSR))
		sp := c.Renewal.Spec
		fmt.Fprintf(&manifest, "%s.csr,%s,%s,%s,%s,%s,%s,%s\n", name, c.DeviceID, csvField(c.DeviceName), c.Target, csvField(sp.Subject.CommonName),
			csvField(strings.Join(sp.DNSNames, " ")), strings.Join(sp.IPAddresses, " "), sp.KeyType)
	}
	f, err := z.Create("manifest.csv")
	if err != nil {
		return nil, err
	}
	f.Write([]byte(manifest.String()))
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func csvField(v string) string {
	v = csvSafe(v)
	if strings.ContainsAny(v, ",\"\n") {
		return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
	}
	return v
}

func fileSafe(v string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, v), "-.")
}

func (s *Server) handleDownloadCSRs(w http.ResponseWriter, r *http.Request) {
	list, err := s.waitingCSRs(r.Context(), principalFrom(r.Context()), selectorFromQuery(r), false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(list) == 0 {
		writeError(w, http.StatusNotFound, "no CSRs are waiting to be signed")
		return
	}
	b, err := csrZip(list)
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="csrs-%s.zip"`, s.now().Format("2006-01-02")))
	w.Write(b)
}

func (s *Server) handleEmailCSRs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var req api.EmailCSRsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if s.cfg.SMTPRelay.Host == "" {
		writeError(w, http.StatusBadRequest, "emailing CSRs needs the server's SMTP relay (-smtp-host)")
		return
	}
	if len(req.Recipients) == 0 || len(req.Recipients) > 20 {
		writeError(w, http.StatusBadRequest, "give 1 to 20 recipients")
		return
	}
	for i, to := range req.Recipients {
		a, err := mail.ParseAddress(strings.TrimSpace(to))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid recipient "+to)
			return
		}
		req.Recipients[i] = a.Address
	}
	if scopeEmpty(req.Selector) && len(req.Selector.DeviceIDs) == 0 && !req.All {
		writeError(w, http.StatusBadRequest, "say which devices (device_ids, labels or channel), or all=true")
		return
	}
	list, err := s.waitingCSRs(ctx, p, req.Selector, true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(list) == 0 {
		writeError(w, http.StatusNotFound, "no CSRs are waiting to be signed on those devices")
		return
	}
	if err := s.emailCSRs(ctx, list, req.Recipients, req.Note, false); err != nil {
		writeError(w, http.StatusBadGateway, "sending failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": len(list)})
}

// emailCSRs sends waiting CSRs (a zip) to whoever signs them, and records
// on each renewal that it was sent (or reminded).
func (s *Server) emailCSRs(ctx context.Context, list []*api.DeviceCert, recipients []string, note string, reminder bool) error {
	zipData, err := csrZip(list)
	if err != nil {
		return err
	}
	now := s.now()
	var body strings.Builder
	csrs := plural(len(list), "certificate signing request")
	if reminder {
		fmt.Fprintf(&body, "Hello,\r\n\r\nA reminder: we're still waiting for the signed certificates for these %s (CSRs).\r\n", csrs)
	} else {
		fmt.Fprintf(&body, "Hello,\r\n\r\nAttached are %s (CSRs) for devices we manage for you.\r\n", csrs)
	}
	fmt.Fprintf(&body, "Please sign each one with your CA and send the certificates back (PEM; one file or many, in any order).\r\n")
	if base := strings.TrimRight(s.cfg.Android.PublicURL, "/"); base != "" {
		fmt.Fprintf(&body, "If you have an account, you can also upload them yourself: %s/#/certificates\r\n", base)
	}
	if note = strings.TrimSpace(note); note != "" {
		fmt.Fprintf(&body, "\r\n%s\r\n", note)
	}
	body.WriteString("\r\n")
	var soonest *time.Time
	for _, c := range list {
		line := fmt.Sprintf("  - %s · %s", c.DeviceName, describeSpec(c.Renewal.Spec))
		if c.Installed != nil {
			line += ", current certificate expires " + c.Installed.NotAfter.Format("2 Jan 2006")
			if soonest == nil || c.Installed.NotAfter.Before(*soonest) {
				t := c.Installed.NotAfter
				soonest = &t
			}
		}
		body.WriteString(line + "\r\n")
	}
	fmt.Fprintf(&body, "\r\nThe private keys stay on the devices; only the requests are attached.\r\n\r\n%s\r\n", s.cfg.ReportBrand)
	subject := fmt.Sprintf("[%s] %s", s.cfg.ReportBrand, csrs)
	if reminder {
		subject = fmt.Sprintf("[%s] Reminder: %s still to sign", s.cfg.ReportBrand, plural(len(list), "certificate"))
	}
	if soonest != nil {
		subject += ", the first expiring " + soonest.Format("2 Jan")
	}
	relay := s.cfg.SMTPRelay
	relay.To = recipients
	sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := smtpDeliver(sendCtx, &relay, mimeWithAttachment(relay.From, recipients, subject, body.String(),
		fmt.Sprintf("csrs-%s.zip", now.Format("2006-01-02")), "application/zip", zipData)); err != nil {
		return err
	}
	for _, c := range list {
		if reminder {
			c.Renewal.RemindedAt = &now
		} else {
			c.Renewal.EmailedAt, c.Renewal.EmailedTo = &now, recipients
		}
		if err := s.store.PutDeviceCert(ctx, c); err != nil {
			return err
		}
	}
	kind, verb := "cert.csrs_emailed", "emailed"
	if reminder {
		kind, verb = "cert.csrs_reminded", "reminder sent"
	}
	s.logEvent(ctx, kind, "", "", fmt.Sprintf("%d CSRs %s to %s", len(list), verb, strings.Join(recipients, ", ")))
	return nil
}

// ---- signed certificates in ----

func (s *Server) handleUploadCerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var req api.UploadCertsRequest
	if !readJSONLimit(w, r, &req, maxCertUpload) {
		return
	}
	all, err := certs.ParseCerts([]byte(req.PEM))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(all) == 0 {
		writeError(w, http.StatusBadRequest, "no PEM certificates found (expected -----BEGIN CERTIFICATE----- blocks)")
		return
	}
	// Renewals waiting for a certificate, by public key — only on devices
	// the uploader operates (others look the same as no match).
	list, devs, err := s.visibleCerts(ctx, p, api.Selector{})
	if err != nil {
		s.internalError(w, err)
		return
	}
	waiting := map[string]*api.DeviceCert{}
	installed := map[string]bool{}
	for _, c := range list {
		if c.Renewal == nil || api.RoleRank(p.roleFor(devs[c.DeviceID])) < api.RoleRank(api.RoleOperator) {
			continue
		}
		switch c.Renewal.Status {
		case api.RenewalCSRReady:
			waiting[c.Renewal.PublicKey] = c
		case api.RenewalInstalling, api.RenewalInstalled:
			installed[c.Renewal.PublicKey] = true
		}
	}
	resp := api.UploadCertsResponse{Installing: []api.DeviceCert{}, Rejected: []api.CertRejected{}}
	now := s.now()
	var wake []string
	for _, leaf := range all {
		key := certs.PublicKeyID(leaf.PublicKey)
		c := waiting[key]
		reject := func(reason string) {
			resp.Rejected = append(resp.Rejected, api.CertRejected{Subject: leaf.Subject.String(), Serial: strings.ToUpper(leaf.SerialNumber.Text(16)), Reason: reason})
		}
		if c == nil {
			switch {
			case leaf.IsCA:
				// Part of a chain: used with the certificates it issued.
			case installed[key]:
				reject("already installed or being installed")
			default:
				reject("doesn't match any waiting CSR (or not one you can manage)")
			}
			continue
		}
		chain, err := checkSignedCert(leaf, all, c.Renewal.Spec, now)
		if err != nil {
			reject(err.Error())
			continue
		}
		d := devs[c.DeviceID]
		spec := api.CertInstallSpec{Target: c.Target, RenewalID: c.Renewal.ID, Certificate: string(certs.EncodeCerts(leaf)), Chain: string(certs.EncodeCerts(chain...))}
		args, _ := json.Marshal(spec)
		a, err := s.queueAction(ctx, d, api.CreateActionRequest{Type: api.ActionCertInstall, Args: []string{string(args)}}, "")
		if err != nil {
			s.internalError(w, err)
			return
		}
		info := certs.Describe(leaf)
		rn := c.Renewal
		rn.Status, rn.Certificate, rn.UploadedBy, rn.UploadedAt, rn.ActionID, rn.UpdatedAt, rn.Message = api.RenewalInstalling, &info, p.name, &now, a.ID, now, ""
		if err := s.store.PutDeviceCert(ctx, c); err != nil {
			s.internalError(w, err)
			return
		}
		s.logEvent(ctx, "cert.uploaded", d.ID, "", fmt.Sprintf("%s: signed %s certificate (serial %s, valid until %s) received; installing",
			d.Name, c.Target, info.Serial, info.NotAfter.Format("2 Jan 2006")))
		delete(waiting, key)
		installed[key] = true
		resp.Installing = append(resp.Installing, *c)
		wake = append(wake, d.ID)
	}
	s.wakeIDs(wake)
	writeJSON(w, http.StatusOK, resp)
}

// checkSignedCert checks a certificate returned for a CSR and finds its
// chain among the uploaded certificates (root CAs left out).
func checkSignedCert(leaf *x509.Certificate, uploaded []*x509.Certificate, spec api.CertCSRSpec, now time.Time) ([]*x509.Certificate, error) {
	switch {
	case now.After(leaf.NotAfter):
		return nil, fmt.Errorf("expired on %s", leaf.NotAfter.Format("2 Jan 2006"))
	case leaf.NotBefore.After(now.Add(24 * time.Hour)):
		return nil, fmt.Errorf("not valid until %s", leaf.NotBefore.Format("2 Jan 2006"))
	case leaf.IsCA:
		return nil, errors.New("this is a CA certificate, not one for the device")
	}
	if len(leaf.ExtKeyUsage) > 0 && !containsEKU(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) && !containsEKU(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return nil, errors.New("not issued for server authentication (extended key usage)")
	}
	// Every name the CSR asked for must be in the certificate.
	if cn := spec.Subject.CommonName; cn != "" && leaf.Subject.CommonName != cn && !containsStr(leaf.DNSNames, cn) {
		return nil, fmt.Errorf("issued for %q, but the CSR asked for %q", leaf.Subject.CommonName, cn)
	}
	for _, n := range spec.DNSNames {
		if !containsStr(leaf.DNSNames, n) {
			return nil, fmt.Errorf("missing the DNS name %s the CSR asked for", n)
		}
	}
	for _, ip := range spec.IPAddresses {
		found := false
		for _, have := range leaf.IPAddresses {
			found = found || have.Equal(net.ParseIP(ip))
		}
		if !found {
			return nil, fmt.Errorf("missing the IP address %s the CSR asked for", ip)
		}
	}
	// Follow issuers through the upload; stop at a root or a missing link.
	var chain []*x509.Certificate
	cur := leaf
	for depth := 0; depth < 8; depth++ {
		var issuer *x509.Certificate
		for _, c := range uploaded {
			if c != cur && bytes.Equal(c.RawSubject, cur.RawIssuer) && cur.CheckSignatureFrom(c) == nil {
				issuer = c
				break
			}
		}
		if issuer == nil {
			for _, c := range uploaded {
				if c != cur && bytes.Equal(c.RawSubject, cur.RawIssuer) {
					return nil, fmt.Errorf("the chain doesn't verify: %q didn't sign it", c.Subject.String())
				}
			}
			break
		}
		if bytes.Equal(issuer.RawSubject, issuer.RawIssuer) {
			break // a root: devices' clients have it already
		}
		chain = append(chain, issuer)
		cur = issuer
	}
	return chain, nil
}

func containsEKU(list []x509.ExtKeyUsage, u x509.ExtKeyUsage) bool {
	for _, x := range list {
		if x == u {
			return true
		}
	}
	return false
}

func mimeWithAttachment(from string, to []string, subject, text, fileName, contentType string, data []byte) string {
	return mimeMessage(from, to, subject, mimePart{"text/plain; charset=utf-8", "", []byte(text)},
		mimePart{contentType, fileName, data})
}
