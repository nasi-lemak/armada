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
	key := newSecret("dk")
	dev := &api.Device{
		ID:         newID("dev"),
		Name:       truncate(name, 128),
		Info:       req.Info,
		Packages:   map[string]string{api.AgentPackage: req.Info.AgentVersion},
		EnrolledAt: now,
		LastSeenAt: now,
	}
	err := s.store.Enroll(r.Context(), hashSecret(req.Token), dev, hashSecret(key), now)
	if errors.Is(err, store.ErrTokenInvalid) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.event(r.Context(), "device.enrolled", dev.ID, "", fmt.Sprintf("%s enrolled (%s/%s, agent %s)", dev.Name, dev.Info.OS, dev.Info.Arch, dev.Info.AgentVersion))
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
	if req.Packages == nil {
		req.Packages = map[string]string{}
	}
	if req.Info.AgentVersion != "" {
		req.Packages[api.AgentPackage] = req.Info.AgentVersion
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
	writeJSON(w, http.StatusOK, resp)
}

// decideUpdate evaluates the active deployments for a device, records any
// progress implied by its reported versions, and returns the update to offer.
func (s *Server) decideUpdate(ctx context.Context, dev *api.Device, busy bool) (*api.UpdateInstruction, error) {
	deps, err := s.store.ActiveDeployments(ctx)
	if err != nil {
		return nil, err
	}
	var cands []*rollout.Candidate
	for _, d := range deps {
		rel, err := s.store.GetRelease(ctx, d.ReleaseID)
		if err != nil {
			return nil, err
		}
		prog, err := s.store.GetDeviceUpdate(ctx, d.ID, dev.ID)
		if errors.Is(err, store.ErrNotFound) {
			prog = nil
		} else if err != nil {
			return nil, err
		}
		cands = append(cands, &rollout.Candidate{Deployment: d, Release: rel, Progress: prog})
	}
	dec := rollout.Decide(dev, cands, busy)
	now := s.now()
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
	return &api.UpdateInstruction{
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
	// Devices may only download releases they have been offered.
	ups, err := s.store.ListDeviceUpdates(r.Context(), "", dev.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	allowed := false
	for _, u := range ups {
		if u.ReleaseID == id {
			allowed = true
			break
		}
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
	res.Output = truncate(res.Output, 64*1024)
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

// handleAgentDownload serves the newest armada-agent release for a platform,
// so new devices can bootstrap with install.sh.
func (s *Server) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dev := &api.Device{Info: api.DeviceInfo{OS: q.Get("os"), Arch: q.Get("arch")}}
	channel := q.Get("channel")
	if channel == "" {
		channel = "stable"
	}
	rels, err := s.store.ListReleases(r.Context(), api.AgentPackage)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var best *api.Release
	for _, rel := range rels {
		if rel.Channel != channel || !rollout.PlatformMatches(rel, dev) {
			continue
		}
		if best == nil || rollout.Compare(rel.Version, best.Version) > 0 {
			best = rel
		}
	}
	if best == nil {
		writeError(w, http.StatusNotFound, "no armada-agent release for this platform and channel")
		return
	}
	s.serveArtifact(w, r, best.ID)
}
