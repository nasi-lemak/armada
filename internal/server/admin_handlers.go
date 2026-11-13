package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// ---- summary ----

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	sum := api.FleetSummary{PackageVersions: map[string]map[string]int{}, Channels: map[string]int{}}
	p := principalFrom(ctx)
	for _, d := range devs {
		if !p.sees(d) {
			continue
		}
		s.decorate(d)
		sum.Devices++
		if d.Status == api.DeviceOnline {
			sum.Online++
		} else {
			sum.Offline++
		}
		sum.Channels[d.Channel]++
		if d.ParentID != "" {
			sum.Peripherals++
		}
		switch d.Health {
		case api.HealthWarning:
			sum.HealthWarning++
		case api.HealthCritical:
			sum.HealthCritical++
		}
		for p, v := range d.Packages {
			if sum.PackageVersions[p] == nil {
				sum.PackageVersions[p] = map[string]int{}
			}
			sum.PackageVersions[p][v]++
		}
	}
	deps, err := s.store.ListDeployments(ctx, api.DeploymentRunning, api.DeploymentPaused)
	if err != nil {
		s.internalError(w, err)
		return
	}
	sum.ActiveDeployments = len(deps)
	rels, err := s.store.ListReleases(ctx, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	sum.Releases = len(rels)
	firing, err := s.store.ListAlerts(ctx, true, 10000)
	if err != nil {
		s.internalError(w, err)
		return
	}
	sum.FiringAlerts = len(s.visibleAlerts(ctx, p, firing))
	if p.scoped() {
		sum.ActiveDeployments, sum.Releases = 0, 0 // fleet-wide; not shown to client accounts
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleSigningKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.PublicKey(), "algorithm": "ed25519"})
}

// ---- devices ----

func (s *Server) decorate(d *api.Device) {
	limit := s.cfg.OfflineAfter
	if d.ParentID != "" {
		// Gateways scan for peripherals every few minutes, not every heartbeat.
		limit = max(limit, s.cfg.PeripheralOfflineAfter)
	}
	if s.now().Sub(d.LastSeenAt) > limit {
		d.Status = api.DeviceOffline
	} else {
		d.Status = api.DeviceOnline
	}
	d.Connected = s.push.connected(d.ID)
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.store.ListDevices(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	q := r.URL.Query()
	sel := api.Selector{Channel: q.Get("channel"), Labels: map[string]string{}}
	for _, l := range q["label"] {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			v = "*"
		}
		sel.Labels[k] = v
	}
	status, health := q.Get("status"), q.Get("health")
	out := []*api.Device{}
	for _, d := range devs {
		s.decorate(d)
		if !principalFrom(r.Context()).sees(d) || !rollout.Matches(sel, d) || (status != "" && d.Status != status) || (health != "" && d.Health != health) {
			continue
		}
		out = append(out, d)
	}
	s.annotateDevices(r.Context(), out...)
	writeJSON(w, http.StatusOK, out)
}

type deviceDetail struct {
	Device      *api.Device         `json:"device"`
	Peripherals []*api.Device       `json:"peripherals"`
	Updates     []*api.DeviceUpdate `json:"updates"`
	Actions     []*api.Action       `json:"actions"`
	Events      []*api.Event        `json:"events"`
	Config      []DeviceConfigView  `json:"config"`
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	s.decorate(d)
	s.annotateDevices(ctx, d)
	det := deviceDetail{Device: d}
	var err error
	if det.Updates, err = s.store.ListDeviceUpdates(ctx, "", d.ID); err != nil {
		s.internalError(w, err)
		return
	}
	if det.Actions, err = s.store.ListActions(ctx, d.ID, 50); err != nil {
		s.internalError(w, err)
		return
	}
	if det.Events, err = s.store.ListEvents(ctx, d.ID, "", 50); err != nil {
		s.internalError(w, err)
		return
	}
	all, err := s.store.ListDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, p := range all {
		if p.ParentID == d.ID && principalFrom(ctx).sees(p) {
			s.decorate(p)
			det.Peripherals = append(det.Peripherals, p)
		}
	}
	if !principalFrom(ctx).scoped() { // profiles are fleet-wide
		profiles, err := s.store.ListConfigProfiles(ctx)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if det.Config, err = s.deviceConfigView(ctx, d, profiles); err != nil {
			s.internalError(w, err)
			return
		}
	}
	det.Updates, det.Actions, det.Events, det.Peripherals, det.Config = nonNil(det.Updates), nonNil(det.Actions), nonNil(det.Events), nonNil(det.Peripherals), nonNil(det.Config)
	writeJSON(w, http.StatusOK, det)
}

func (s *Server) loadDevice(w http.ResponseWriter, r *http.Request) (*api.Device, bool) {
	d, err := s.store.GetDevice(r.Context(), r.PathValue("id"))
	if err == nil && !principalFrom(r.Context()).sees(d) {
		err = store.ErrNotFound // out of scope looks the same as missing
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return d, true
}

type patchDeviceRequest struct {
	Name         *string           `json:"name"`
	Channel      *string           `json:"channel"`
	Labels       map[string]string `json:"labels"`        // replaces all labels when non-nil
	SetLabels    map[string]string `json:"set_labels"`    // merged into labels
	RemoveLabels []string          `json:"remove_labels"` // deleted from labels
	// LicenseRevoked revokes (true) or reinstates (false) the device's app licenses.
	LicenseRevoked *bool `json:"license_revoked"`
}

var labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

func validLabels(m map[string]string) error {
	for k, v := range m {
		if !labelKeyRe.MatchString(k) {
			return fmt.Errorf("invalid label key %q", k)
		}
		if len(v) > 256 {
			return fmt.Errorf("label %q value too long", k)
		}
	}
	return nil
}

func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if !needDeviceRole(w, r, d, api.RoleOperator) {
		return
	}
	var req patchDeviceRequest
	if !readJSON(w, r, &req) {
		return
	}
	// Licensing is the vendor's anti-redistribution control: only fleet-wide
	// operators can lift a revocation (or impose one).
	if req.LicenseRevoked != nil && *req.LicenseRevoked != d.LicenseRevoked && api.RoleRank(principalFrom(r.Context()).fleetRole()) < api.RoleRank(api.RoleOperator) {
		writeError(w, http.StatusForbidden, "only fleet-wide accounts can change app licensing")
		return
	}
	if req.Name != nil {
		d.Name = truncate(strings.TrimSpace(*req.Name), 128)
	}
	if req.Channel != nil {
		if !channelRe.MatchString(*req.Channel) {
			writeError(w, http.StatusBadRequest, "invalid channel")
			return
		}
		d.Channel = *req.Channel
	}
	if req.Labels != nil {
		d.Labels = req.Labels
	}
	for k, v := range req.SetLabels {
		d.Labels[k] = v
	}
	for _, k := range req.RemoveLabels {
		delete(d.Labels, k)
	}
	if err := validLabels(d.Labels); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if api.RoleRank(principalFrom(r.Context()).roleFor(d)) < api.RoleRank(api.RoleOperator) {
		writeError(w, http.StatusForbidden, "this change would move the device out of the devices you can manage")
		return
	}
	if err := s.store.UpdateDeviceMeta(r.Context(), d.ID, d.Name, d.Channel, d.Labels); err != nil {
		s.internalError(w, err)
		return
	}
	if req.LicenseRevoked != nil && *req.LicenseRevoked != d.LicenseRevoked {
		if err := s.store.SetLicenseRevoked(r.Context(), d.ID, *req.LicenseRevoked); err != nil {
			s.internalError(w, err)
			return
		}
		d.LicenseRevoked = *req.LicenseRevoked
		verb := "reinstated"
		if d.LicenseRevoked {
			verb = "revoked; apps stop working when their current license expires"
		}
		s.event(r.Context(), "device.license", d.ID, "", fmt.Sprintf("%s: licenses %s", d.Name, verb))
	}
	s.decorate(d)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteDevice(r.Context(), d.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.event(r.Context(), "device.deleted", "", "", fmt.Sprintf("%s (%s) was removed from the fleet", d.Name, d.ID))
	w.WriteHeader(http.StatusNoContent)
}

// ---- actions ----

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	acts, err := s.store.ListActions(r.Context(), d.ID, 100)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if acts == nil {
		acts = []*api.Action{}
	}
	writeJSON(w, http.StatusOK, acts)
}

func (s *Server) handleCreateAction(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok || !needDeviceRole(w, r, d, api.RoleOperator) {
		return
	}
	var req api.CreateActionRequest
	if !readJSON(w, r, &req) {
		return
	}
	switch req.Type {
	case api.ActionReboot, api.ActionRestartAgent:
		req.Args = nil
	case api.ActionExec:
		if len(req.Args) == 0 {
			writeError(w, http.StatusBadRequest, "exec requires args (the command and its arguments)")
			return
		}
	case api.ActionLogs:
		if err := validLogArgs(req.Args); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case api.ActionLock, api.ActionWipe:
		if !strings.EqualFold(d.Info.OS, "android") {
			writeError(w, http.StatusBadRequest, req.Type+" is only available on Android devices")
			return
		}
		if req.Type == api.ActionLock {
			req.Args = nil
		} else if len(req.Args) != 1 || req.Args[0] != "confirm="+d.ID {
			writeError(w, http.StatusBadRequest, "wipe erases the device: confirm with args [\"confirm="+d.ID+"\"]")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown action type; use reboot, restart-agent, exec, logs, lock or wipe")
		return
	}
	if d.ParentID != "" {
		writeError(w, http.StatusBadRequest, "peripherals run no agent; send actions to their gateway")
		return
	}
	// Running commands as root and factory-resetting devices you supplied
	// are vendor powers: per-client operators can reboot, restart, lock
	// and read logs, but not these.
	if (req.Type == api.ActionExec || req.Type == api.ActionWipe) && api.RoleRank(principalFrom(r.Context()).fleetRole()) < api.RoleRank(api.RoleOperator) {
		writeError(w, http.StatusForbidden, req.Type+" needs the operator role on the whole fleet")
		return
	}
	now := s.now()
	a := &api.Action{ID: newID("act"), DeviceID: d.ID, Type: req.Type, Args: req.Args, Status: api.ActionQueued, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAction(r.Context(), a); err != nil {
		s.internalError(w, err)
		return
	}
	s.event(r.Context(), "action.queued", d.ID, "", fmt.Sprintf("%s queued for %s", describeAction(a), d.Name))
	s.push.wake(d.ID) // a connected agent runs it within a second or two
	writeJSON(w, http.StatusCreated, a)
}

func describeAction(a *api.Action) string {
	switch a.Type {
	case api.ActionExec:
		return "exec `" + truncate(strings.Join(a.Args, " "), 80) + "`"
	case api.ActionLogs:
		return "logs (" + truncate(strings.Join(a.Args, " "), 80) + ")"
	case api.ActionWipe:
		return "factory reset (wipe)"
	}
	return a.Type
}

var (
	logUnitRe = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]{1,128}$`)
	logNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// validLogArgs checks a logs action: a source, then key=value options.
func validLogArgs(args []string) error {
	if len(args) == 0 || (args[0] != "journal" && args[0] != "file") {
		return errors.New(`logs needs a source: "journal" (systemd journal) or "file" (a log file named in the agent's log_files)`)
	}
	seen := map[string]string{}
	for _, a := range args[1:] {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("logs option %q must be key=value", a)
		}
		seen[k] = v
		switch k {
		case "unit":
			if !logUnitRe.MatchString(v) {
				return errors.New("invalid unit name")
			}
		case "name":
			if !logNameRe.MatchString(v) {
				return errors.New("invalid log file name")
			}
		case "lines":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 5000 {
				return errors.New("lines must be 1-5000")
			}
		case "since":
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > 31*24*time.Hour {
				return errors.New("since must be a duration such as 30m, 6h or 72h (up to 31 days)")
			}
		case "grep":
			if len(v) > 200 {
				return errors.New("grep pattern too long")
			}
		default:
			return fmt.Errorf("unknown logs option %q (use unit, name, lines, since, grep)", k)
		}
	}
	if args[0] == "file" && seen["name"] == "" {
		return errors.New("file logs need name=<log file name from the agent's log_files>")
	}
	return nil
}

// ---- enrollment tokens ----

var channelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListTokens(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	if ts == nil {
		ts = []*api.EnrollmentToken{}
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req api.CreateTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if !channelRe.MatchString(req.Channel) {
		writeError(w, http.StatusBadRequest, "invalid channel")
		return
	}
	if req.Labels == nil {
		req.Labels = map[string]string{}
	}
	// Without a fleet-wide operator grant, people can only enroll devices
	// where they are operators. With several such grants (e.g. two
	// customers), the labels must pick one.
	if p := principalFrom(r.Context()); api.RoleRank(p.fleetRole()) < api.RoleRank(api.RoleOperator) {
		var cands []api.Selector
		for _, g := range p.grants {
			if !scopeEmpty(g.Scope) && api.RoleRank(g.Role) >= api.RoleRank(api.RoleOperator) {
				cands = append(cands, g.Scope)
			}
		}
		if len(cands) == 0 {
			writeError(w, http.StatusForbidden, "enrolling devices needs the operator role on some devices")
			return
		}
		sc := cands[0]
		if len(cands) > 1 {
			found := false
			for _, cand := range cands {
				ok := true
				for k, v := range cand.Labels {
					ok = ok && req.Labels[k] == v
				}
				if ok && len(cand.Labels) > 0 {
					sc, found = cand, true
					break
				}
			}
			if !found {
				var opts []string
				for _, cand := range cands {
					opts = append(opts, selText(cand))
				}
				writeError(w, http.StatusBadRequest, "say which devices this token is for: add the labels of one of "+strings.Join(opts, "; "))
				return
			}
		}
		for k, v := range sc.Labels {
			if v != "*" {
				req.Labels[k] = v
			}
		}
		if sc.Channel != "" {
			req.Channel = sc.Channel
		}
	}
	if err := validLabels(req.Labels); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MaxUses < 0 || req.TTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, "max_uses and ttl_seconds must not be negative")
		return
	}
	now := s.now()
	t := &api.EnrollmentToken{
		ID:          newID("tok"),
		Description: truncate(req.Description, 256),
		Channel:     req.Channel,
		Labels:      req.Labels,
		MaxUses:     req.MaxUses,
		CreatedAt:   now,
		Token:       newSecret("et"),
	}
	if req.TTLSeconds > 0 {
		exp := now.Add(time.Duration(req.TTLSeconds) * time.Second)
		t.ExpiresAt = &exp
	}
	if err := s.store.CreateToken(r.Context(), t, hashSecret(t.Token)); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.RevokeToken(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- releases ----

var (
	packageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	platRe    = regexp.MustCompile(`^[a-z0-9_]{0,32}$`)
)

func (s *Server) handleListReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := s.store.ListReleases(r.Context(), r.URL.Query().Get("package"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	if rels == nil {
		rels = []*api.Release{}
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *Server) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	rel, err := s.store.GetRelease(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "release not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

func (s *Server) handleAdminArtifact(w http.ResponseWriter, r *http.Request) {
	s.serveArtifact(w, r, r.PathValue("id"))
}

type uploadResult struct {
	Release    *api.Release    `json:"release"`
	Deployment *api.Deployment `json:"deployment,omitempty"`
}

// handleUploadRelease accepts multipart/form-data with metadata fields
// (package, version, channel, os, arch, notes, auto_deploy) followed by a
// "file" part. The file is streamed to disk, hashed and signed.
func (s *Server) handleUploadRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		writeError(w, http.StatusUnsupportedMediaType, "expected multipart/form-data")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxArtifactBytes+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	rel := &api.Release{ID: newID("rel"), Channel: "stable", CreatedAt: now}
	autoDeploy := false
	var tmpPath string
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FormName() == "file" {
			if tmpPath != "" {
				writeError(w, http.StatusBadRequest, "only one file may be uploaded")
				return
			}
			f, err := os.CreateTemp(s.artifactDir, "upload-*")
			if err != nil {
				s.internalError(w, err)
				return
			}
			tmpPath = f.Name()
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(part, s.cfg.MaxArtifactBytes+1))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, "upload failed: "+err.Error())
				return
			}
			if n > s.cfg.MaxArtifactBytes {
				writeError(w, http.StatusRequestEntityTooLarge, "artifact too large")
				return
			}
			rel.Size = n
			rel.SHA256 = hex.EncodeToString(h.Sum(nil))
			continue
		}
		val, err := io.ReadAll(io.LimitReader(part, 64*1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		v := strings.TrimSpace(string(val))
		switch part.FormName() {
		case "package":
			rel.Package = v
		case "version":
			rel.Version = v
		case "channel":
			if v != "" {
				rel.Channel = v
			}
		case "os":
			rel.OS = strings.ToLower(v)
		case "arch":
			rel.Arch = strings.ToLower(v)
		case "notes":
			rel.Notes = truncate(v, 8192)
		case "auto_deploy":
			autoDeploy, _ = strconv.ParseBool(v)
		case "version_code":
			if v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil || n <= 0 {
					writeError(w, http.StatusBadRequest, "version_code must be a positive integer")
					return
				}
				rel.VersionCode = n
			}
		}
	}
	switch {
	case tmpPath == "":
		writeError(w, http.StatusBadRequest, "missing file part")
		return
	case !packageRe.MatchString(rel.Package):
		writeError(w, http.StatusBadRequest, "invalid or missing package name (letters, digits, . _ -)")
		return
	case !versionRe.MatchString(rel.Version):
		writeError(w, http.StatusBadRequest, "invalid or missing version")
		return
	case !channelRe.MatchString(rel.Channel):
		writeError(w, http.StatusBadRequest, "invalid channel")
		return
	case !platRe.MatchString(rel.OS) || !platRe.MatchString(rel.Arch):
		writeError(w, http.StatusBadRequest, "invalid os or arch")
		return
	}
	rel.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.signKey, api.SigningPayload(rel.Package, rel.Version, rel.SHA256)))
	if err := os.Rename(tmpPath, s.artifactPath(rel.ID)); err != nil {
		s.internalError(w, err)
		return
	}
	tmpPath = ""
	if err := s.store.CreateRelease(ctx, rel); err != nil {
		os.Remove(s.artifactPath(rel.ID))
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "a release with this package, version and platform already exists")
			return
		}
		s.internalError(w, err)
		return
	}
	s.event(ctx, "release.created", "", "", fmt.Sprintf("release %s %s uploaded to %s (%s)", rel.Package, rel.Version, rel.Channel, humanBytes(rel.Size)))
	if rel.Size >= 1<<20 { // precompute the delta-update index off the request path
		s.bg.Add(1)
		go func(rel api.Release) {
			defer s.bg.Done()
			if _, err := s.chunkIndex(&rel); err != nil {
				s.log.Warn("chunk index", "release", rel.ID, "err", err)
			}
		}(*rel)
	}
	res := uploadResult{Release: rel}
	if autoDeploy {
		d, err := s.createDeployment(ctx, api.CreateDeploymentRequest{
			Name:      fmt.Sprintf("auto: %s %s → %s", rel.Package, rel.Version, rel.Channel),
			ReleaseID: rel.ID,
			Selector:  api.Selector{Channel: rel.Channel},
		})
		if err != nil {
			s.internalError(w, err)
			return
		}
		res.Deployment = d
	}
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleDeleteRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.DeleteRelease(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "release not found")
		return
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	os.Remove(s.artifactPath(id))
	os.Remove(s.chunkPath(id))
	w.WriteHeader(http.StatusNoContent)
}

// ---- deployments ----

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	var statuses []string
	if st := r.URL.Query().Get("status"); st != "" {
		statuses = strings.Split(st, ",")
	}
	deps, err := s.store.ListDeployments(r.Context(), statuses...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if deps == nil {
		deps = []*api.Deployment{}
	}
	for _, d := range deps {
		s.markApproval(d)
	}
	writeJSON(w, http.StatusOK, deps)
}

func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	var req api.CreateDeploymentRequest
	if !readJSON(w, r, &req) {
		return
	}
	d, err := s.createDeployment(r.Context(), req)
	var ve validationError
	if errors.As(err, &ve) {
		writeError(w, http.StatusBadRequest, ve.Error())
		return
	} else if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "release not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

type validationError struct{ error }

func (s *Server) createDeployment(ctx context.Context, req api.CreateDeploymentRequest) (*api.Deployment, error) {
	rel, err := s.store.GetRelease(ctx, req.ReleaseID)
	if err != nil {
		return nil, err
	}
	strat := s.cfg.DefaultStrategy
	strat.Stages = append([]int(nil), strat.Stages...)
	if req.Strategy != nil {
		strat = *req.Strategy
	}
	if err := rollout.ValidateStrategy(&strat); err != nil {
		return nil, validationError{err}
	}
	if err := validLabels(req.Selector.Labels); err != nil {
		return nil, validationError{err}
	}
	if req.Selector.Channel != "" && !channelRe.MatchString(req.Selector.Channel) {
		return nil, validationError{errors.New("invalid channel")}
	}
	now := s.now()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = fmt.Sprintf("%s %s", rel.Package, rel.Version)
	}
	status := api.DeploymentRunning
	createdBy := ""
	if p := principalFrom(ctx); p != nil {
		createdBy = p.name
	}
	if s.cfg.RolloutApproval != "" && strat.Stages[0] > s.cfg.UnapprovedMaxPercent {
		// The canary would already be too big: wait for approval before
		// offering anything.
		status = api.DeploymentAwaitingApproval
	}
	if strat.StartAt != nil {
		if strat.StartAt.After(now.AddDate(1, 0, 0)) {
			return nil, validationError{errors.New("start_at is more than a year away")}
		}
		if strat.StartAt.After(now) {
			if status != api.DeploymentAwaitingApproval {
				status = api.DeploymentScheduled
			}
		} else {
			strat.StartAt = nil
		}
	}
	d := &api.Deployment{
		ID:             newID("dep"),
		Name:           truncate(name, 128),
		ReleaseID:      rel.ID,
		Package:        rel.Package,
		Version:        rel.Version,
		Selector:       req.Selector,
		Strategy:       strat,
		Status:         status,
		CreatedBy:      createdBy,
		StageStartedAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateDeployment(ctx, d); err != nil {
		return nil, err
	}
	s.markApproval(d)
	if status == api.DeploymentAwaitingApproval {
		s.event(ctx, "deployment.created", "", d.ID, fmt.Sprintf("deployment %q created by %s: %s %s, waiting for approval by someone else (first stage %d%% is above the %d%% allowed before approval)",
			d.Name, createdBy, d.Package, d.Version, strat.Stages[0], s.cfg.UnapprovedMaxPercent))
	} else if status == api.DeploymentScheduled {
		s.event(ctx, "deployment.scheduled", "", d.ID, fmt.Sprintf("deployment %q scheduled for %s: %s %s, stages %v%s",
			d.Name, strat.StartAt.UTC().Format("2006-01-02 15:04 UTC"), d.Package, d.Version, strat.Stages, describeWindows(strat)))
	} else {
		s.event(ctx, "deployment.created", "", d.ID, fmt.Sprintf("deployment %q started: %s %s, stages %v%s", d.Name, d.Package, d.Version, strat.Stages, describeWindows(strat)))
	}
	return d, nil
}

func describeWindows(st api.Strategy) string {
	if st.Windows == "" && len(st.Days) == 0 {
		return ""
	}
	w := st.Windows
	if w == "" {
		w = "all day"
	}
	if len(st.Days) > 0 {
		w += " on " + strings.Join(st.Days, ",")
	}
	tz := st.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	return ", only " + w + " " + tz
}

// startScheduled starts deployments whose start time has come.
func (s *Server) startScheduled(ctx context.Context, now time.Time) error {
	deps, err := s.store.ListDeployments(ctx, api.DeploymentScheduled)
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.Strategy.StartAt != nil && d.Strategy.StartAt.After(now) {
			continue
		}
		d.Status, d.StatusReason, d.StageStartedAt, d.UpdatedAt = api.DeploymentRunning, "", now, now
		if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
			return err
		}
		s.event(ctx, "deployment.started", "", d.ID, fmt.Sprintf("scheduled deployment %q started: %s %s", d.Name, d.Package, d.Version))
	}
	return nil
}

type deploymentDetail struct {
	Deployment *api.Deployment     `json:"deployment"`
	Release    *api.Release        `json:"release"`
	Updates    []*api.DeviceUpdate `json:"updates"`
	Targeted   int                 `json:"targeted"` // devices matching selector & platform
	Eligible   int                 `json:"eligible"` // of those, in the current stage
	Events     []*api.Event        `json:"events"`
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.store.GetDeployment(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.markApproval(d)
	det := deploymentDetail{Deployment: d}
	if det.Release, err = s.store.GetRelease(ctx, d.ReleaseID); err != nil {
		s.internalError(w, err)
		return
	}
	if det.Updates, err = s.store.ListDeviceUpdates(ctx, d.ID, ""); err != nil {
		s.internalError(w, err)
		return
	}
	det.Updates = nonNil(det.Updates)
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, dev := range devs {
		if rollout.Matches(d.Selector, dev) && rollout.PlatformMatches(det.Release, dev) {
			det.Targeted++
			if rollout.InStage(d, dev.ID) {
				det.Eligible++
			}
		}
	}
	if det.Events, err = s.store.ListEvents(ctx, "", d.ID, 100); err != nil {
		s.internalError(w, err)
		return
	}
	det.Events = nonNil(det.Events)
	writeJSON(w, http.StatusOK, det)
}

func (s *Server) handleDeploymentOp(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, err := s.store.GetDeployment(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "deployment not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	now := s.now()
	op := r.PathValue("op")
	if d.Status == api.DeploymentAborted || d.Status == api.DeploymentCompleted {
		writeError(w, http.StatusConflict, "deployment is already "+d.Status)
		return
	}
	if d.Status == api.DeploymentScheduled && op != "abort" && op != "resume" && op != "approve" {
		writeError(w, http.StatusConflict, "deployment is scheduled; resume starts it now, abort cancels it")
		return
	}
	if d.Status == api.DeploymentAwaitingApproval && op != "abort" && op != "approve" {
		writeError(w, http.StatusConflict, "deployment is waiting for approval; approve or abort it")
		return
	}
	if op == "advance" && s.needsApproval(d) {
		writeError(w, http.StatusConflict, "this rollout can't go past its first stage until someone other than "+d.CreatedBy+" approves it")
		return
	}
	switch op {
	case "approve":
		if !s.needsApproval(d) {
			writeError(w, http.StatusConflict, "this rollout doesn't need approval")
			return
		}
		approver := principalFrom(ctx).name
		if approver == d.CreatedBy {
			writeError(w, http.StatusForbidden, "someone other than the person who started this rollout must approve it")
			return
		}
		d.ApprovedBy, d.ApprovedAt = approver, &now
		if d.Status == api.DeploymentAwaitingApproval {
			d.Status, d.StageStartedAt = api.DeploymentRunning, now
			if d.Strategy.StartAt != nil && d.Strategy.StartAt.After(now) {
				d.Status = api.DeploymentScheduled
			}
		}
		if strings.HasPrefix(d.StatusReason, "waiting for approval") {
			d.StatusReason = ""
		}
	case "pause":
		d.Status, d.StatusReason = api.DeploymentPaused, "paused by admin"
	case "resume":
		// Resuming acknowledges the failures so far; the failure-rate check
		// only counts new ones from here on.
		d.Status, d.StatusReason = api.DeploymentRunning, ""
		d.AcknowledgedFailures = d.Stats.Failed + d.Stats.RolledBack
		d.StageStartedAt = now // restart the soak clock
	case "retry":
		n, err := s.store.ResetFailedUpdates(ctx, d.ID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		d.AcknowledgedFailures = 0
		s.event(ctx, "deployment.retry", "", d.ID, fmt.Sprintf("deployment %q: %d failed device(s) will be retried", d.Name, n))
	case "abort":
		d.Status, d.StatusReason = api.DeploymentAborted, "aborted by admin"
	case "complete":
		d.Status, d.StatusReason = api.DeploymentCompleted, "marked complete by admin"
	case "advance":
		if d.StageIndex >= len(d.Strategy.Stages)-1 {
			writeError(w, http.StatusConflict, "deployment is already at its final stage")
			return
		}
		d.StageIndex++
		d.StageStartedAt = now
	default:
		writeError(w, http.StatusNotFound, "unknown operation; use approve, pause, resume, advance, retry, complete or abort")
		return
	}
	d.UpdatedAt = now
	if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
		s.internalError(w, err)
		return
	}
	switch op {
	case "approve":
		s.event(ctx, "deployment.approved", "", d.ID, fmt.Sprintf("deployment %q (started by %s) approved by %s", d.Name, d.CreatedBy, d.ApprovedBy))
	case "advance":
		s.event(ctx, "deployment.advance", "", d.ID, fmt.Sprintf("deployment %q advanced to %d%% by admin", d.Name, d.CurrentPercent()))
	case "retry":
	default:
		s.event(ctx, "deployment."+op, "", d.ID, fmt.Sprintf("deployment %q: %s by admin", d.Name, op))
	}
	if d, err = s.store.GetDeployment(ctx, d.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.markApproval(d)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) evaluateDeployment(ctx context.Context, d *api.Deployment, now time.Time) error {
	ev := rollout.Evaluate(d, int64(now.Sub(d.StageStartedAt).Seconds()))
	switch {
	case ev.Pause:
		d.Status, d.StatusReason, d.UpdatedAt = api.DeploymentPaused, ev.Reason, now
		if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
			return err
		}
		s.log.Warn("deployment auto-paused", "deployment", d.ID, "reason", ev.Reason)
		s.event(ctx, "deployment.paused", "", d.ID, fmt.Sprintf("deployment %q auto-paused: %s", d.Name, ev.Reason))
	case ev.Advance && !rollout.WindowOpen(d.Strategy, now):
		// Soak time is up but the rollout window is closed: advance when it opens.
	case ev.Advance && s.needsApproval(d):
		reason := fmt.Sprintf("waiting for approval to go past %d%% (someone other than %s)", d.CurrentPercent(), d.CreatedBy)
		if d.StatusReason != reason {
			d.StatusReason, d.UpdatedAt = reason, now
			if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
				return err
			}
			s.event(ctx, "deployment.waiting", "", d.ID, fmt.Sprintf("deployment %q finished its canary and is waiting for approval", d.Name))
		}
	case ev.Advance:
		d.StageIndex++
		d.StageStartedAt, d.UpdatedAt = now, now
		if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
			return err
		}
		s.event(ctx, "deployment.advanced", "", d.ID, fmt.Sprintf("deployment %q advanced to %d%%", d.Name, d.CurrentPercent()))
	}
	return nil
}

// ---- events ----

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	p := principalFrom(r.Context())
	fetch := limit
	if p.scoped() {
		fetch = limit * 10 // filtered below
	}
	evs, err := s.store.ListEvents(r.Context(), q.Get("device_id"), q.Get("deployment_id"), fetch)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if p.scoped() {
		visible, err := s.visibleDeviceIDs(r.Context(), p)
		if err != nil {
			s.internalError(w, err)
			return
		}
		kept := evs[:0]
		for _, e := range evs {
			if visible[e.DeviceID] && len(kept) < limit {
				kept = append(kept, e)
			}
		}
		evs = kept
	}
	if evs == nil {
		evs = []*api.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

// event records an activity-log entry and triggers event-based alerts.
func (s *Server) event(ctx context.Context, typ, deviceID, deploymentID, msg string) {
	s.logEvent(ctx, typ, deviceID, deploymentID, msg)
	s.alertOnEvent(context.WithoutCancel(ctx), typ, deviceID, deploymentID, msg)
}

// logEvent only records an activity-log entry.
func (s *Server) logEvent(ctx context.Context, typ, deviceID, deploymentID, msg string) {
	actor := ""
	if p := principalFrom(ctx); p != nil {
		actor = p.name
	}
	if err := s.store.AddEvent(context.WithoutCancel(ctx), api.Event{Time: s.now(), Type: typ, DeviceID: deviceID, DeploymentID: deploymentID, Message: msg, Actor: actor}); err != nil {
		s.log.Error("record event", "err", err)
	}
}

// visibleDeviceIDs is the set of devices a principal may see.
func (s *Server) visibleDeviceIDs(ctx context.Context, p *principal) (map[string]bool, error) {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range devs {
		if p.sees(d) {
			out[d.ID] = true
		}
	}
	return out, nil
}

// visibleAlerts drops alerts about devices outside the principal's scope
// (and fleet-wide alerts, for client accounts).
func (s *Server) visibleAlerts(ctx context.Context, p *principal, alerts []*api.Alert) []*api.Alert {
	if !p.scoped() {
		return alerts
	}
	visible, err := s.visibleDeviceIDs(ctx, p)
	if err != nil {
		return nil
	}
	var out []*api.Alert
	for _, a := range alerts {
		if visible[a.DeviceID] {
			out = append(out, a)
		}
	}
	return out
}

// needsApproval reports whether a rollout still needs a second person's
// approval before it may go past its first stage.
func (s *Server) needsApproval(d *api.Deployment) bool {
	return s.cfg.RolloutApproval != "" && d.ApprovedBy == "" &&
		d.Status != api.DeploymentAborted && d.Status != api.DeploymentCompleted
}

func (s *Server) markApproval(d *api.Deployment) { d.NeedsApproval = s.needsApproval(d) }
