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
	for _, d := range devs {
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
	sum.FiringAlerts = len(firing)
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
		if !rollout.Matches(sel, d) || (status != "" && d.Status != status) || (health != "" && d.Health != health) {
			continue
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

type deviceDetail struct {
	Device      *api.Device         `json:"device"`
	Peripherals []*api.Device       `json:"peripherals"`
	Updates     []*api.DeviceUpdate `json:"updates"`
	Actions     []*api.Action       `json:"actions"`
	Events      []*api.Event        `json:"events"`
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	s.decorate(d)
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
		if p.ParentID == d.ID {
			s.decorate(p)
			det.Peripherals = append(det.Peripherals, p)
		}
	}
	det.Updates, det.Actions, det.Events, det.Peripherals = nonNil(det.Updates), nonNil(det.Actions), nonNil(det.Events), nonNil(det.Peripherals)
	writeJSON(w, http.StatusOK, det)
}

func (s *Server) loadDevice(w http.ResponseWriter, r *http.Request) (*api.Device, bool) {
	d, err := s.store.GetDevice(r.Context(), r.PathValue("id"))
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
	var req patchDeviceRequest
	if !readJSON(w, r, &req) {
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
	acts, err := s.store.ListActions(r.Context(), r.PathValue("id"), 100)
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
	if !ok {
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
	default:
		writeError(w, http.StatusBadRequest, "unknown action type; use reboot, restart-agent or exec")
		return
	}
	now := s.now()
	a := &api.Action{ID: newID("act"), DeviceID: d.ID, Type: req.Type, Args: req.Args, Status: api.ActionQueued, CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateAction(r.Context(), a); err != nil {
		s.internalError(w, err)
		return
	}
	s.event(r.Context(), "action.queued", d.ID, "", fmt.Sprintf("%s queued for %s", describeAction(a), d.Name))
	writeJSON(w, http.StatusCreated, a)
}

func describeAction(a *api.Action) string {
	if a.Type == api.ActionExec {
		return "exec `" + truncate(strings.Join(a.Args, " "), 80) + "`"
	}
	return a.Type
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
	d := &api.Deployment{
		ID:             newID("dep"),
		Name:           truncate(name, 128),
		ReleaseID:      rel.ID,
		Package:        rel.Package,
		Version:        rel.Version,
		Selector:       req.Selector,
		Strategy:       strat,
		Status:         api.DeploymentRunning,
		StageStartedAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateDeployment(ctx, d); err != nil {
		return nil, err
	}
	s.event(ctx, "deployment.created", "", d.ID, fmt.Sprintf("deployment %q started: %s %s, stages %v", d.Name, d.Package, d.Version, strat.Stages))
	return d, nil
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
	switch op {
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
		writeError(w, http.StatusNotFound, "unknown operation; use pause, resume, advance, retry, complete or abort")
		return
	}
	d.UpdatedAt = now
	if err := s.store.UpdateDeploymentState(ctx, d); err != nil {
		s.internalError(w, err)
		return
	}
	switch op {
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
	evs, err := s.store.ListEvents(r.Context(), q.Get("device_id"), q.Get("deployment_id"), limit)
	if err != nil {
		s.internalError(w, err)
		return
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
	if err := s.store.AddEvent(context.WithoutCancel(ctx), api.Event{Time: s.now(), Type: typ, DeviceID: deviceID, DeploymentID: deploymentID, Message: msg}); err != nil {
		s.log.Error("record event", "err", err)
	}
}
