package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	now := s.now()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Info.Hostname
	}
	if name == "" {
		name = "device"
	}
	att, hwKey, err := s.verifyEnrollment(r.Context(), &req)
	if err != nil {
		s.log.Warn("enrollment rejected", "host", req.Info.Hostname, "err", err)
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	key := newSecret("dk")
	dev := &api.Device{
		ID:          newID("dev"),
		Name:        truncate(name, 128),
		Info:        req.Info,
		Packages:    map[string]string{s.agentPackageFor(req.Info): req.Info.AgentVersion},
		EnrolledAt:  now,
		LastSeenAt:  now,
		Attestation: att,
	}
	err = s.store.Enroll(r.Context(), hashSecret(req.Token), dev, hashSecret(key), hwKey, now)
	if errors.Is(err, store.ErrTokenInvalid) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	msg := fmt.Sprintf("%s enrolled (%s/%s, agent %s)", dev.Name, dev.Info.OS, dev.Info.Arch, dev.Info.AgentVersion)
	if att != nil {
		msg += fmt.Sprintf(", hardware key in %s, boot %s", att.SecurityLevel, att.BootState)
	}
	s.event(r.Context(), "device.enrolled", dev.ID, "", msg)
	writeJSON(w, http.StatusOK, api.EnrollResponse{
		DeviceID:                 dev.ID,
		DeviceKey:                key,
		SigningPublicKey:         s.PublicKey(),
		HeartbeatIntervalSeconds: int(s.cfg.HeartbeatInterval.Seconds()),
	})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	var req api.HeartbeatRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	now := s.now()
	s.push.checkedIn(dev.ID)
	if req.Packages == nil {
		req.Packages = map[string]string{}
	}
	if req.Info.AgentVersion != "" {
		req.Packages[s.agentPackageFor(req.Info)] = req.Info.AgentVersion
	}
	wasOffline := now.Sub(dev.LastSeenAt) > s.cfg.OfflineAfter
	if err := s.store.RecordHeartbeat(ctx, dev.ID, req.Info, req.Metrics, req.Packages, truncate(req.Status, 512), now); err != nil {
		s.internalError(w, err)
		return
	}
	if wasOffline {
		s.event(ctx, "device.online", dev.ID, "", dev.Name+" came back online")
	}
	dev.Info, dev.Metrics, dev.Packages = req.Info, req.Metrics, req.Packages
	if req.Health != nil {
		if err := s.recordHealth(ctx, dev, req.Health); err != nil {
			s.internalError(w, err)
			return
		}
	}

	s.recordHistory(ctx, dev, now)

	resp := api.HeartbeatResponse{HeartbeatIntervalSeconds: int(s.cfg.HeartbeatInterval.Seconds())}

	update, err := s.decideUpdate(ctx, dev, req.Busy)
	if err != nil {
		s.internalError(w, err)
		return
	}
	resp.Update = update

	actions, err := s.store.ClaimQueuedActions(ctx, dev.ID, now)
	if err != nil {
		s.internalError(w, err)
		return
	}
	resp.Actions = actions
	if resp.Monitors, err = s.monitorsFor(ctx, dev); err != nil {
		s.internalError(w, err)
		return
	}
	if len(req.Peripherals) > 0 {
		if resp.PeripheralUpdates, err = s.handlePeripherals(ctx, dev, req.Peripherals); err != nil {
			s.internalError(w, err)
			return
		}
	}
	if strings.EqualFold(dev.Info.OS, "android") {
		resp.ManagedPackages = s.managedPackages(r)
		resp.Policy = &api.DevicePolicy{DisableDebugging: s.cfg.Android.DisableDebugging, BlockUninstall: true}
	}
	writeJSON(w, http.StatusOK, resp)
}

// decideUpdate evaluates the active deployments for a device, records any
// progress implied by its reported versions, and returns the update to offer.
func (s *Server) decideUpdate(ctx context.Context, dev *api.Device, busy bool) (*api.UpdateInstruction, error) {
	now := s.now()
	deps, err := s.store.ActiveDeployments(ctx)
	if err != nil {
		return nil, err
	}
	isAndroid := strings.EqualFold(dev.Info.OS, "android")
	var androidApps map[string]bool
	if isAndroid {
		apps, err := s.store.ListApps(ctx)
		if err != nil {
			return nil, err
		}
		androidApps = map[string]bool{}
		for _, a := range apps {
			androidApps[a.Package] = true
		}
	}
	var cands []*rollout.Candidate
	for _, d := range deps {
		rel, err := s.store.GetRelease(ctx, d.ReleaseID)
		if err != nil {
			return nil, err
		}
		// Android devices can only verify and install registered apps (the
		// agent needs their signing certificates); other devices never get
		// Android releases, even ones uploaded without an OS.
		if isAndroid && !androidApps[rel.Package] {
			continue
		}
		if !isAndroid && strings.EqualFold(rel.OS, "android") {
			continue
		}
		// Peripheral firmware (OS "ble") only goes to peripherals, and peripherals only get firmware.
		if (dev.Info.OS == api.PeripheralOS) != strings.EqualFold(rel.OS, api.PeripheralOS) {
			continue
		}
		// Firmware is per hardware model: never offer one model's image to another,
		// even when the device has no version of that package yet.
		if dev.Info.OS == api.PeripheralOS && rel.Package != dev.Info.Model {
			continue
		}
		prog, err := s.store.GetDeviceUpdate(ctx, d.ID, dev.ID)
		if errors.Is(err, store.ErrNotFound) {
			prog = nil
		} else if err != nil {
			return nil, err
		}
		cands = append(cands, &rollout.Candidate{Deployment: d, Release: rel, Progress: prog, Closed: !rollout.WindowOpen(d.Strategy, now)})
	}
	dec := rollout.Decide(dev, cands, busy)
	for _, c := range dec.Confirmed {
		c.Progress.Status = api.UpdateSucceeded
		c.Progress.Message = "device reports version " + c.Release.Version
		c.Progress.UpdatedAt = now
		if err := s.store.UpsertDeviceUpdate(ctx, c.Progress); err != nil {
			return nil, err
		}
		s.event(ctx, "update.succeeded", dev.ID, c.Deployment.ID, fmt.Sprintf("%s updated %s to %s", dev.Name, c.Release.Package, c.Release.Version))
	}
	if dec.Offer == nil {
		return nil, nil
	}
	c := dec.Offer
	if c.Progress == nil {
		u := &api.DeviceUpdate{
			DeploymentID: c.Deployment.ID,
			DeviceID:     dev.ID,
			ReleaseID:    c.Release.ID,
			Package:      c.Release.Package,
			Version:      c.Release.Version,
			FromVersion:  dev.Packages[c.Release.Package],
			Status:       api.UpdatePending,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := s.store.UpsertDeviceUpdate(ctx, u); err != nil {
			return nil, err
		}
	}
	var signers []string
	if app, err := s.store.GetApp(ctx, c.Release.Package); err == nil {
		signers = app.Signers
	}
	return &api.UpdateInstruction{
		VersionCode:  c.Release.VersionCode,
		Signers:      signers,
		DeploymentID: c.Deployment.ID,
		ReleaseID:    c.Release.ID,
		Package:      c.Release.Package,
		Version:      c.Release.Version,
		SHA256:       c.Release.SHA256,
		Size:         c.Release.Size,
		Signature:    c.Release.Signature,
		ArtifactPath: "/api/v1/agent/releases/" + c.Release.ID + "/artifact",
	}, nil
}

func (s *Server) handleUpdateReport(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	var rep api.UpdateReport
	if !readJSON(w, r, &rep) {
		return
	}
	switch rep.Status {
	case api.UpdateDownloading, api.UpdateInstalling, api.UpdateSucceeded, api.UpdateFailed, api.UpdateRolledBack:
	default:
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	ctx := r.Context()
	if rep.DeviceID != "" && rep.DeviceID != dev.ID {
		// A gateway reporting for one of its peripherals.
		p, err := s.store.GetDevice(ctx, rep.DeviceID)
		if err != nil || p.ParentID != dev.ID {
			writeError(w, http.StatusForbidden, "device is not a peripheral of this gateway")
			return
		}
		dev = p
	}
	u, err := s.store.GetDeviceUpdate(ctx, rep.DeploymentID, dev.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such update for this device")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if u.Status == api.UpdateSucceeded && rep.Status != api.UpdateSucceeded {
		// A late progress report must not undo a confirmed success.
		writeJSON(w, http.StatusOK, u)
		return
	}
	prev := u.Status
	u.Status, u.Message, u.UpdatedAt = rep.Status, truncate(rep.Message, 4096), s.now()
	if err := s.store.UpsertDeviceUpdate(ctx, u); err != nil {
		s.internalError(w, err)
		return
	}
	if prev != u.Status && api.IsTerminalUpdateStatus(u.Status) {
		msg := fmt.Sprintf("%s: %s %s → %s", dev.Name, u.Package, u.Version, u.Status)
		if u.Message != "" {
			msg += " (" + u.Message + ")"
		}
		s.event(ctx, "update."+u.Status, dev.ID, u.DeploymentID, msg)
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleAgentArtifact(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	id := r.PathValue("id")
	// Devices may only download releases offered to them or their peripherals.
	allowed, err := s.store.ReleaseOfferedToGateway(r.Context(), id, dev.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "release not offered to this device")
		return
	}
	s.serveArtifact(w, r, id)
}

func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request, id string) {
	rel, err := s.store.GetRelease(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "release not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	f, err := os.Open(s.artifactPath(rel.ID))
	if err != nil {
		s.internalError(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s"`, safeName(rel.Package), safeName(rel.Version)))
	w.Header().Set("X-Armada-SHA256", rel.SHA256)
	// ServeContent gives us Range support so agents can resume large downloads.
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (s *Server) artifactPath(releaseID string) string {
	return filepath.Join(s.artifactDir, filepath.Base(releaseID))
}

func (s *Server) handleActionResult(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	var res api.ActionResult
	if !readJSON(w, r, &res) {
		return
	}
	switch res.Status {
	case api.ActionSucceeded, api.ActionFailed, api.ActionRejected:
	default:
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	res.Output = truncateHead(res.Output, 256*1024) // keep the newest output
	id := r.PathValue("id")
	if err := s.store.CompleteAction(r.Context(), id, dev.ID, res, s.now()); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "action not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	a, _ := s.store.GetAction(r.Context(), id)
	if a != nil {
		s.event(r.Context(), "action."+res.Status, dev.ID, "", fmt.Sprintf("%s: action %s %s (exit %d)", dev.Name, a.Type, res.Status, res.ExitCode))
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentDownload serves the newest agent release for a platform, so new
// devices can bootstrap: install.sh for Linux, the provisioning QR for Android.
// Only agent packages are served without authentication.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pkg := q.Get("package")
	if pkg == "" {
		pkg = api.AgentPackage
	}
	if pkg != api.AgentPackage && pkg != s.cfg.Android.AgentPackage {
		writeError(w, http.StatusNotFound, "only agent packages can be downloaded without authentication")
		return
	}
	best, err := s.newestRelease(r, pkg, q.Get("os"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no "+pkg+" release for this platform and channel")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.serveArtifact(w, r, best.ID)
}

// agentPackageFor is the package name under which a device's agent version is
// tracked: the Linux agent and the Android agent are different packages, so a
// rollout of one is never offered to devices running the other.
func (s *Server) agentPackageFor(info api.DeviceInfo) string {
	if strings.EqualFold(info.OS, "android") {
		return s.cfg.Android.AgentPackage
	}
	return api.AgentPackage
}
