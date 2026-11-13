package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Device lifecycle: active → spare / repair → back to active, or retired
// for good. Replacing a device (RMA) moves its identity — name, channel and
// labels, and with them its client, configuration, alert rules and
// rollouts — onto a replacement in one step.
//
// These are fleet operations (they move devices between clients and revoke
// credentials), so they need a fleet-wide operator.

// lifecycleLabel is how states read in events.
var lifecycleLabel = map[string]string{
	api.LifecycleActive: "in service", api.LifecycleSpare: "spare", api.LifecycleRepair: "in repair", api.LifecycleRetired: "retired",
}

func (s *Server) handleSetLifecycle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.LifecycleRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.State, req.Note = strings.ToLower(strings.TrimSpace(req.State)), strings.TrimSpace(req.Note)
	if !api.ValidLifecycle(req.State) {
		writeError(w, http.StatusBadRequest, "state must be active, spare, repair or retired")
		return
	}
	if len(req.Note) > 1000 {
		writeError(w, http.StatusBadRequest, "note is limited to 1000 characters")
		return
	}
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if err := checkLifecycle(d); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.changeLifecycle(ctx, d, req.State, req.Note); err != nil {
		s.internalError(w, err)
		return
	}
	s.decorate(d)
	writeJSON(w, http.StatusOK, d)
}

func checkLifecycle(d *api.Device) error {
	if d.Lifecycle == api.LifecycleRetired {
		return errors.New("this device is retired; to use the hardware again, enroll it again")
	}
	return nil
}

// changeLifecycle moves a checked device to a new state and logs it.
func (s *Server) changeLifecycle(ctx context.Context, d *api.Device, state, note string) error {
	from := d.Lifecycle
	d.Lifecycle = state
	d.LifecycleInfo = s.lifecycleChange(ctx, d.LifecycleInfo, note)
	if err := s.store.SetDeviceLifecycle(ctx, d); err != nil {
		return err
	}
	msg := fmt.Sprintf("%s: %s → %s", d.Name, lifecycleLabel[from], lifecycleLabel[d.Lifecycle])
	if note != "" {
		msg += " (" + note + ")"
	}
	s.logEvent(ctx, "device.lifecycle", d.ID, "", msg)
	if d.Lifecycle == api.LifecycleRetired {
		s.push.wake(d.ID) // drop its live connection: its key no longer works
	}
	return nil
}

// lifecycleChange records who changed a lifecycle and why, keeping the
// replacement links.
func (s *Server) lifecycleChange(ctx context.Context, old *api.LifecycleInfo, note string) *api.LifecycleInfo {
	li := &api.LifecycleInfo{Note: note, ChangedAt: s.now(), ChangedBy: principalFrom(ctx).name}
	if old != nil {
		li.Replaces, li.ReplacedBy = old.Replaces, old.ReplacedBy
	}
	return li
}

func (s *Server) handleReplaceDevice(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.ReplaceRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	if req.OldState == "" {
		req.OldState = api.LifecycleRepair
	}
	switch req.OldState {
	case api.LifecycleRepair, api.LifecycleSpare, api.LifecycleRetired:
	default:
		writeError(w, http.StatusBadRequest, "old_state must be repair, spare or retired")
		return
	}
	old, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if req.ReplacementID == "" || req.ReplacementID == old.ID {
		writeError(w, http.StatusBadRequest, "replacement_id must name another device")
		return
	}
	repl, err := s.store.GetDevice(ctx, req.ReplacementID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown replacement device")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	switch {
	case old.Lifecycle == api.LifecycleRetired:
		writeError(w, http.StatusConflict, "this device is already retired")
		return
	case repl.Lifecycle != api.LifecycleActive && repl.Lifecycle != api.LifecycleSpare:
		writeError(w, http.StatusConflict, "the replacement must be a spare or active device, not "+lifecycleLabel[repl.Lifecycle])
		return
	case (old.ParentID == "") != (repl.ParentID == ""):
		writeError(w, http.StatusBadRequest, "replace a peripheral with a peripheral, and an agent device with an agent device")
		return
	}

	// The replacement takes over the old device's place in the fleet.
	oldName := old.Name
	repl.Name, repl.Channel, repl.Labels = old.Name, old.Channel, old.Labels
	repl.Lifecycle = api.LifecycleActive
	repl.LifecycleInfo = s.lifecycleChange(ctx, repl.LifecycleInfo, req.Note)
	repl.LifecycleInfo.Replaces = old.ID
	// The old device leaves it: no labels means it no longer belongs to a
	// client, rollout or configuration profile.
	old.Name = oldName + " (replaced)"
	old.Labels = map[string]string{}
	old.Lifecycle = req.OldState
	old.LifecycleInfo = s.lifecycleChange(ctx, old.LifecycleInfo, req.Note)
	old.LifecycleInfo.ReplacedBy = repl.ID
	if err := s.store.SetDeviceLifecycle(ctx, old, repl); err != nil {
		s.internalError(w, err)
		return
	}
	msg := fmt.Sprintf("%s replaced by device %s; the old device is now %s", oldName, repl.ID, lifecycleLabel[old.Lifecycle])
	if req.Note != "" {
		msg += " (" + req.Note + ")"
	}
	s.logEvent(ctx, "device.replaced", old.ID, "", msg)
	s.logEvent(ctx, "device.replacement", repl.ID, "", fmt.Sprintf("now %s, replacing device %s", oldName, old.ID))
	// Its new labels may bring configuration and updates: check in now.
	s.push.wake(repl.ID)
	if old.Lifecycle == api.LifecycleRetired {
		s.push.wake(old.ID)
	}
	s.decorate(repl)
	writeJSON(w, http.StatusOK, repl)
}

// notRetired refuses actions on retired devices, which can't receive them.
func notRetired(w http.ResponseWriter, d *api.Device) bool {
	if d.Lifecycle == api.LifecycleRetired {
		writeError(w, http.StatusConflict, "this device is retired")
		return false
	}
	return true
}
