package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Bulk operations apply one change to many devices, with the same rules as
// one device at a time: each device is checked on its own (role, platform,
// scope) and skipped with a reason if the change doesn't apply to it.
//
// Two safeguards against a wrong selector hitting the whole fleet: an
// empty selector needs all=true, and a real run must say how many devices
// it expects to match (from its dry run). If that number has changed, the
// request is refused and nothing happens.

const (
	maxBulkDevices    = 10000
	bulkRetention     = 90 * 24 * time.Hour
	bulkListLimit     = 50
	bulkWakeRatePause = 20 * time.Millisecond // about 50 devices a second
)

// bulkOp is one kind of bulk operation.
type bulkOp struct {
	summary string
	// check says why the operation doesn't apply to a device ("" if it does).
	check func(d *api.Device) string
	// apply makes the change; it returns the ID of an action it queued, if any.
	apply func(ctx context.Context, d *api.Device, batch string) (string, error)
	// wake means devices should check in afterwards.
	wake bool
}

func (s *Server) bulkOperation(p *principal, req *api.BulkRequest) (*bulkOp, int, error) {
	bad := func(format string, a ...any) (*bulkOp, int, error) {
		return nil, http.StatusBadRequest, fmt.Errorf(format, a...)
	}
	fleetOperator := api.RoleRank(p.fleetRole()) >= api.RoleRank(api.RoleOperator)
	patchOp := func(summary string, patch patchDeviceRequest) *bulkOp {
		return &bulkOp{summary: summary,
			check: func(d *api.Device) string {
				c := *d
				if _, err := applyPatch(p, &c, patch); err != nil {
					return err.Error()
				}
				return ""
			},
			apply: func(ctx context.Context, d *api.Device, _ string) (string, error) {
				if _, err := applyPatch(p, d, patch); err != nil {
					return "", err
				}
				return "", s.store.UpdateDeviceMeta(ctx, d.ID, d.Name, d.Channel, d.Labels)
			}}
	}

	switch req.Operation {
	case api.ActionReboot, api.ActionRestartAgent, api.ActionLock, api.ActionExec:
		if req.Operation == api.ActionExec {
			if len(req.Args) == 0 {
				return bad("exec needs args: the command and its arguments")
			}
			if !fleetOperator {
				return nil, http.StatusForbidden, errors.New("exec needs the operator role on the whole fleet")
			}
		}
		summary := describeAction(&api.Action{Type: req.Operation, Args: req.Args})
		return &bulkOp{summary: summary, wake: true,
			check: func(d *api.Device) string {
				ar := api.CreateActionRequest{Type: req.Operation, Args: append([]string(nil), req.Args...)}
				if _, err := checkAction(p, d, &ar); err != nil {
					return err.Error()
				}
				return ""
			},
			apply: func(ctx context.Context, d *api.Device, batch string) (string, error) {
				ar := api.CreateActionRequest{Type: req.Operation, Args: append([]string(nil), req.Args...)}
				if _, err := checkAction(p, d, &ar); err != nil {
					return "", err
				}
				a, err := s.queueAction(ctx, d, ar, batch)
				if err != nil {
					return "", err
				}
				return a.ID, nil
			}}, 0, nil
	case api.ActionWipe:
		return bad("wipe erases a device: wipe devices one at a time")
	case api.ActionLogs:
		return bad("fetch logs from one device at a time")
	case api.BulkWake:
		return &bulkOp{summary: "check in now", wake: true,
			check: func(d *api.Device) string {
				if d.ParentID != "" {
					return "peripherals check in through their gateway"
				}
				return ""
			},
			apply: func(context.Context, *api.Device, string) (string, error) { return "", nil }}, 0, nil
	case api.BulkSetLabels:
		if len(req.Labels) == 0 {
			return bad("set_labels needs labels")
		}
		if err := validLabels(req.Labels); err != nil {
			return bad("%v", err)
		}
		return patchOp("set labels "+fmtSelectorLabels(req.Labels), patchDeviceRequest{SetLabels: req.Labels}), 0, nil
	case api.BulkRemoveLabels:
		if len(req.LabelKeys) == 0 {
			return bad("remove_labels needs label_keys")
		}
		return patchOp("remove labels "+strings.Join(req.LabelKeys, ", "), patchDeviceRequest{RemoveLabels: req.LabelKeys}), 0, nil
	case api.BulkSetChannel:
		if !channelRe.MatchString(req.Channel) {
			return bad("set_channel needs a valid channel")
		}
		ch := req.Channel
		return patchOp("set channel "+ch, patchDeviceRequest{Channel: &ch}), 0, nil
	case api.BulkLifecycle:
		if !api.ValidLifecycle(req.State) {
			return bad("lifecycle needs state: active, spare, repair or retired")
		}
		if !fleetOperator {
			return nil, http.StatusForbidden, errors.New("lifecycle changes need the operator role on the whole fleet")
		}
		return &bulkOp{summary: "lifecycle → " + lifecycleLabel[req.State],
			check: func(d *api.Device) string {
				if err := checkLifecycle(d); err != nil {
					return err.Error()
				}
				if d.Lifecycle == req.State {
					return "already " + lifecycleLabel[req.State]
				}
				return ""
			},
			apply: func(ctx context.Context, d *api.Device, _ string) (string, error) {
				return "", s.changeLifecycle(ctx, d, req.State, req.Note)
			}}, 0, nil
	}
	return bad("unknown operation; use reboot, restart-agent, lock, exec, wake, set_labels, remove_labels, set_channel or lifecycle")
}

func fmtSelectorLabels(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

func (s *Server) handleBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	var req api.BulkRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Note = strings.TrimSpace(req.Note)
	op, code, err := s.bulkOperation(p, &req)
	if err != nil {
		writeError(w, code, err.Error())
		return
	}
	if scopeEmpty(req.Selector) && len(req.Selector.DeviceIDs) == 0 && !req.All {
		writeError(w, http.StatusBadRequest, "say which devices (device_ids, labels or channel), or all=true for every device you can see")
		return
	}
	if !req.DryRun && req.Expect == nil {
		writeError(w, http.StatusBadRequest, "set expect to the number of devices the dry run matched")
		return
	}
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var targets []*api.Device
	for _, d := range devs {
		if p.sees(d) && rollout.Matches(req.Selector, d) {
			targets = append(targets, d)
		}
	}
	if len(targets) > maxBulkDevices {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%d devices match; bulk operations are limited to %d", len(targets), maxBulkDevices))
		return
	}
	if !req.DryRun && *req.Expect != len(targets) {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d devices match now, not %d: preview it again", len(targets), *req.Expect))
		return
	}

	b := &api.BulkOperation{Operation: req.Operation, Summary: op.summary, DryRun: req.DryRun, Matched: len(targets),
		Applied: []api.BulkTarget{}, Skipped: []api.BulkTarget{}, CreatedBy: p.name, CreatedAt: s.now()}
	if !req.DryRun {
		b.ID = newID("blk")
	}
	var wake []string
	for _, d := range targets {
		t := api.BulkTarget{ID: d.ID, Name: d.Name}
		if api.RoleRank(p.roleFor(d)) < api.RoleRank(api.RoleOperator) {
			t.Reason = "needs the operator role on this device"
		} else {
			t.Reason = op.check(d)
		}
		if t.Reason != "" {
			b.Skipped = append(b.Skipped, t)
			continue
		}
		if !req.DryRun {
			actionID, err := op.apply(ctx, d, b.ID)
			if err != nil {
				s.log.Error("bulk operation", "batch", b.ID, "device", d.ID, "err", err)
				t.Reason = "failed: " + err.Error()
				b.Skipped = append(b.Skipped, t)
				continue
			}
			if actionID != "" {
				t.ActionID, t.Status = actionID, api.ActionQueued
			}
			if op.wake {
				wake = append(wake, d.ID)
			}
		}
		b.Applied = append(b.Applied, t)
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, b)
		return
	}
	if err := s.store.PutBulkOperation(ctx, b); err != nil {
		s.internalError(w, err)
		return
	}
	msg := fmt.Sprintf("bulk %s on %d devices", op.summary, len(b.Applied))
	if len(b.Skipped) > 0 {
		msg += fmt.Sprintf(" (%d skipped)", len(b.Skipped))
	}
	s.logEvent(ctx, "bulk."+req.Operation, "", "", msg+" — "+b.ID)
	s.wakeIDs(wake)
	s.addBulkProgress(ctx, b)
	writeJSON(w, http.StatusCreated, b)
}

// wakeIDs wakes connected devices, spread out so a large batch doesn't
// check in all at once.
func (s *Server) wakeIDs(ids []string) {
	var live []string
	for _, id := range ids {
		if s.push.connected(id) {
			live = append(live, id)
		}
	}
	if len(live) == 0 {
		return
	}
	go func() {
		for _, id := range live {
			s.push.wake(id)
			select {
			case <-s.push.done:
				return
			case <-time.After(bulkWakeRatePause):
			}
		}
	}()
}

// addBulkProgress fills in the status of each queued action.
func (s *Server) addBulkProgress(ctx context.Context, b *api.BulkOperation) {
	if b.ID == "" {
		return
	}
	st, err := s.store.BatchActionStatus(ctx, b.ID)
	if err != nil || len(st) == 0 {
		return
	}
	b.Progress = map[string]int{}
	for i := range b.Applied {
		t := &b.Applied[i]
		if t.ActionID == "" {
			continue
		}
		if status, ok := st[t.ActionID]; ok {
			t.Status = status
		} else {
			t.Status = "removed" // the device was deleted
		}
		b.Progress[t.Status]++
	}
}

// bulkVisible: fleet-wide accounts see every bulk operation; client
// accounts see their own.
func bulkVisible(p *principal, b *api.BulkOperation) bool {
	return !p.scoped() || b.CreatedBy == p.name
}

func (s *Server) handleListBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	all, err := s.store.ListBulkOperations(ctx, 500)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := []*api.BulkOperation{}
	for _, b := range all {
		if !bulkVisible(p, b) {
			continue
		}
		s.addBulkProgress(ctx, b)
		out = append(out, b)
		if len(out) == bulkListLimit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetBulk(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	b, err := s.store.GetBulkOperation(ctx, r.PathValue("id"))
	if err == nil && !bulkVisible(principalFrom(ctx), b) {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "bulk operation not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.addBulkProgress(ctx, b)
	writeJSON(w, http.StatusOK, b)
}
