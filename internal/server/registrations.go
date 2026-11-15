package server

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Pre-registration and bulk import.
//
// An import is a CSV with one row per device, keyed by serial number (or a
// "device" column naming an enrolled device by ID or name). Rows for
// enrolled devices update them now; rows for serials not enrolled yet
// become pre-registrations, applied when a device with that serial
// enrolls. Every row is checked first: if any has an error, nothing is
// changed.

const (
	importMaxRows  = 5000
	importMaxBytes = 4 << 20
)

// clientObjectAccess is who sees and manages an object that belongs to a
// client (scope) or to the vendor (empty scope): fleet-wide accounts see
// all and their operators manage all; client accounts see and (as
// operators) manage their own client's.
func clientObjectAccess(p *principal, scope api.Selector) (see, edit bool) {
	if r := p.fleetRole(); r != "" {
		return true, api.RoleRank(r) >= api.RoleRank(api.RoleOperator)
	}
	if scopeEmpty(scope) {
		return false, false
	}
	for _, g := range p.grants {
		if sameScope(g.Scope, scope) {
			see = true
			edit = edit || api.RoleRank(g.Role) >= api.RoleRank(api.RoleOperator)
		}
	}
	return see, edit
}

// importScope is the client a new registration belongs to: none for
// fleet-wide operators, else the caller's client (named with ?client=).
func importScope(p *principal, r *http.Request) (api.Selector, error) {
	return chooseClientScope(p, r.URL.Query().Get("client"), "pre-registrations")
}

// chooseClientScope is the client a new object belongs to: none for
// fleet-wide operators unless they name one ("customer=acme"), else the
// caller's client (which they must name if they have several).
func chooseClientScope(p *principal, client, what string) (api.Selector, error) {
	var sc api.Selector
	if c := client; c != "" {
		labels, err := parseLabelList(c)
		if err != nil {
			return sc, err
		}
		sc.Labels = labels
	}
	if api.RoleRank(p.fleetRole()) >= api.RoleRank(api.RoleOperator) {
		return sc, nil
	}
	return pickClientScope(p, sc, false, what)
}

// applyRegistration fills in an enrolling device from its registration.
// The token has already set the labels, and those win; the registration's
// channel, being specific to the device, replaces the token's.
func applyRegistration(reg *api.Registration, d *api.Device, tok *api.EnrollmentToken) bool {
	if !scopeEmpty(reg.ClientScope) && !rollout.Matches(reg.ClientScope, d) {
		return false // a client's registration only names its own devices
	}
	if scopeEmpty(reg.ClientScope) && tok != nil && !scopeEmpty(tok.ClientScope) {
		// A vendor registration isn't for devices a client enrolls: anyone
		// can report any serial number, so a client could otherwise claim it.
		return false
	}
	if reg.Name != "" {
		d.Name = reg.Name
	}
	if reg.Channel != "" {
		prev := d.Channel
		d.Channel = reg.Channel
		if !scopeEmpty(reg.ClientScope) && !rollout.Matches(reg.ClientScope, d) {
			d.Channel = prev // it must stay among the client's devices
		}
	}
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	for k, v := range reg.Labels {
		if _, fromToken := d.Labels[k]; !fromToken {
			d.Labels[k] = v
		}
	}
	if reg.Inventory != nil && !reg.Inventory.Empty() {
		inv := *reg.Inventory
		d.Inventory = &inv
	}
	return true
}

// parseLabelList reads "k=v; k2=v2" (also comma- or newline-separated).
func parseLabelList(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("labels must look like key=value; got %q", part)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out, validLabels(out)
}

// importRecord is one parsed CSV row.
type importRecord struct {
	line           int
	serial, device string
	name, channel  *string
	labels         map[string]string
	inv            api.Inventory // empty fields: leave as is
	hasInv         bool
}

var importColumns = map[string]string{
	"serial": "serial", "serial_number": "serial", "serial number": "serial",
	"device": "device", "device_id": "device", "id": "device",
	"name": "name", "channel": "channel", "labels": "labels",
	"asset_tag": "asset_tag", "asset tag": "asset_tag", "location": "location", "contact": "contact", "notes": "notes",
}

func parseImport(body io.Reader) ([]importRecord, error) {
	cr := csv.NewReader(body)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	head, err := cr.Read()
	if err == io.EOF {
		return nil, errors.New("the file is empty")
	} else if err != nil {
		return nil, fmt.Errorf("CSV: %w", err)
	}
	cols := make([]string, len(head))
	keyed := false
	for i, h := range head {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		switch {
		case importColumns[h] != "":
			cols[i] = importColumns[h]
		case strings.HasPrefix(h, "label:") || strings.HasPrefix(h, "label."):
			k := strings.TrimSpace(strings.TrimSpace(head[i])[6:])
			if !labelKeyRe.MatchString(k) {
				return nil, fmt.Errorf("invalid label column %q", head[i])
			}
			cols[i] = "label:" + k
		case h == "":
			cols[i] = "" // ignored
		default:
			return nil, fmt.Errorf("unknown column %q (use serial, device, name, channel, labels, label:KEY, asset_tag, location, contact, notes)", head[i])
		}
		keyed = keyed || cols[i] == "serial" || cols[i] == "device"
	}
	if !keyed {
		return nil, errors.New("the first row must name the columns, including serial (or device)")
	}
	var out []importRecord
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(out) == importMaxRows {
			return nil, fmt.Errorf("at most %d rows per import", importMaxRows)
		}
		r := importRecord{line: line, labels: map[string]string{}}
		blank := true
		for i, v := range rec {
			if i >= len(cols) || cols[i] == "" {
				continue
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue // empty cells change nothing
			}
			blank = false
			switch c := cols[i]; c {
			case "serial":
				r.serial = v
			case "device":
				r.device = v
			case "name":
				r.name = &v
			case "channel":
				r.channel = &v
			case "labels":
				m, err := parseLabelList(v)
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", line, err)
				}
				maps.Copy(r.labels, m)
			case "asset_tag":
				r.inv.AssetTag, r.hasInv = v, true
			case "location":
				r.inv.Location, r.hasInv = v, true
			case "contact":
				r.inv.Contact, r.hasInv = v, true
			case "notes":
				r.inv.Notes, r.hasInv = v, true
			default:
				r.labels[strings.TrimPrefix(c, "label:")] = v
			}
		}
		if !blank {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the file has no rows")
	}
	return out, nil
}

// importPlan is what applying an import will write.
type importPlan struct {
	devices []*api.Device // updated devices (name, channel, labels, inventory)
	changes [][]string
	regs    []*api.Registration
}

func (s *Server) handleImportRegistrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	scope, err := importScope(p, r)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	recs, err := parseImport(http.MaxBytesReader(w, r.Body, importMaxBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, plan, err := s.planImport(ctx, p, scope, recs)
	if err != nil {
		s.internalError(w, err)
		return
	}
	res.DryRun = r.URL.Query().Get("dry_run") != ""
	if res.DryRun || res.Errors > 0 {
		writeJSON(w, http.StatusOK, res)
		return
	}
	for i, d := range plan.devices {
		if err := s.store.UpdateDeviceMeta(ctx, d.ID, d.Name, d.Channel, d.Labels); err != nil {
			s.internalError(w, err)
			return
		}
		if err := s.store.SetDeviceInventory(ctx, d.ID, d.Inventory); err != nil {
			s.internalError(w, err)
			return
		}
		s.logEvent(ctx, "device.imported", d.ID, "", d.Name+": "+strings.Join(plan.changes[i], ", ")+" (import)")
	}
	for _, reg := range plan.regs {
		if err := s.store.PutRegistration(ctx, reg); err != nil {
			s.internalError(w, err)
			return
		}
	}
	res.Applied = true
	s.logEvent(ctx, "registration.import", "", "", fmt.Sprintf("import: %s updated, %s pre-registered",
		plural(res.Updated, "device"), plural(res.Created, "serial")))
	writeJSON(w, http.StatusOK, res)
}

// planImport checks every row and works out what it would change.
func (s *Server) planImport(ctx context.Context, p *principal, scope api.Selector, recs []importRecord) (*api.ImportResult, *importPlan, error) {
	devs, err := s.store.ListAllDevices(ctx)
	if err != nil {
		return nil, nil, err
	}
	bySerial, byKey := map[string][]*api.Device{}, map[string][]*api.Device{}
	for _, d := range devs {
		if !p.sees(d) {
			continue // other clients' devices don't exist here
		}
		byKey[d.ID] = append(byKey[d.ID], d)
		byKey[d.Name] = append(byKey[d.Name], d)
		if sn := store.RegistrationID(d.Info.Serial); sn != "" && d.Lifecycle != api.LifecycleRetired {
			bySerial[sn] = append(bySerial[sn], d)
		}
	}
	res := &api.ImportResult{Rows: []api.ImportRow{}}
	plan := &importPlan{}
	seen := map[string]int{}
	now := s.now()
	for _, rec := range recs {
		row := api.ImportRow{Line: rec.line, Serial: rec.serial}
		fail := func(format string, args ...any) {
			row.Action, row.Error = "error", fmt.Sprintf(format, args...)
		}
		key := "serial:" + store.RegistrationID(rec.serial)
		if rec.device != "" {
			key = "device:" + rec.device
		}
		var targets []*api.Device
		switch {
		case seen[key] != 0:
			fail("line %d already covers this device", seen[key])
		case rec.device != "":
			targets = byKey[rec.device]
			if len(targets) == 0 {
				fail("no device %q", rec.device)
			}
		default:
			targets = bySerial[store.RegistrationID(rec.serial)]
		}
		seen[key] = rec.line
		if row.Action == "" && len(targets) > 1 {
			fail("%d devices match; use the device column with an ID", len(targets))
		}
		if row.Action == "" && len(targets) == 1 {
			s.planDeviceRow(p, targets[0], rec, &row, plan)
		} else if row.Action == "" {
			s.planRegistrationRow(ctx, p, scope, rec, &row, plan, now)
		}
		switch row.Action {
		case "updated":
			res.Updated++
		case "registered":
			res.Created++
		case "error":
			res.Errors++
		}
		res.Rows = append(res.Rows, row)
	}
	return res, plan, nil
}

func (s *Server) planDeviceRow(p *principal, cur *api.Device, rec importRecord, row *api.ImportRow, plan *importPlan) {
	row.DeviceID, row.Device = cur.ID, cur.Name
	if api.RoleRank(p.roleFor(cur)) < api.RoleRank(api.RoleOperator) {
		row.Action, row.Error = "error", "you can't change this device"
		return
	}
	d := *cur
	d.Labels = maps.Clone(cur.Labels)
	req := patchDeviceRequest{Name: rec.name, Channel: rec.channel, SetLabels: rec.labels}
	if _, err := applyPatch(p, &d, req); err != nil {
		row.Action, row.Error = "error", err.Error()
		return
	}
	var changes []string
	if d.Name != cur.Name {
		changes = append(changes, fmt.Sprintf("name %s → %s", cur.Name, d.Name))
	}
	if d.Channel != cur.Channel {
		changes = append(changes, fmt.Sprintf("channel %s → %s", cmp.Or(cur.Channel, "none"), d.Channel))
	}
	for _, k := range slices.Sorted(maps.Keys(d.Labels)) {
		if old, ok := cur.Labels[k]; !ok || old != d.Labels[k] {
			changes = append(changes, k+"="+d.Labels[k])
		}
	}
	if rec.hasInv {
		inv := api.Inventory{}
		if cur.Inventory != nil {
			inv = *cur.Inventory
		}
		before := inv
		for _, f := range []struct {
			dst *string
			v   string
		}{{&inv.AssetTag, rec.inv.AssetTag}, {&inv.Location, rec.inv.Location},
			{&inv.Contact, rec.inv.Contact}, {&inv.Notes, rec.inv.Notes}} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
		if err := cleanInventory(&inv); err != nil {
			row.Action, row.Error = "error", err.Error()
			return
		}
		if inv != before {
			changes = append(changes, inventoryChanges(before, inv)...)
		}
		d.Inventory = &inv
	}
	row.Changes = changes
	if len(changes) == 0 {
		row.Action = "unchanged"
		return
	}
	row.Action = "updated"
	plan.devices = append(plan.devices, &d)
	plan.changes = append(plan.changes, changes)
}

func inventoryChanges(a, b api.Inventory) []string {
	var out []string
	for _, f := range []struct{ name, a, b string }{{"asset tag", a.AssetTag, b.AssetTag}, {"location", a.Location, b.Location},
		{"contact", a.Contact, b.Contact}, {"notes", a.Notes, b.Notes}} {
		if f.a != f.b {
			v := f.b
			if len(v) > 40 {
				v = v[:40] + "…"
			}
			out = append(out, f.name+" "+v)
		}
	}
	return out
}

func (s *Server) planRegistrationRow(ctx context.Context, p *principal, scope api.Selector, rec importRecord, row *api.ImportRow, plan *importPlan, now time.Time) {
	fail := func(msg string) { row.Action, row.Error = "error", msg }
	if rec.serial == "" {
		fail("no serial number")
		return
	}
	if len(rec.serial) > 128 {
		fail("serial number too long")
		return
	}
	reg := &api.Registration{Serial: strings.TrimSpace(rec.serial), Labels: rec.labels, ClientScope: scope, CreatedBy: p.name, CreatedAt: now}
	if rec.name != nil {
		reg.Name = truncate(*rec.name, 128)
	}
	if rec.channel != nil {
		if !channelRe.MatchString(*rec.channel) {
			fail("invalid channel")
			return
		}
		reg.Channel = *rec.channel
	}
	for k, v := range scope.Labels {
		if got, ok := reg.Labels[k]; ok && got != v && v != "*" {
			fail(fmt.Sprintf("label %s must be %s for your devices", k, v))
			return
		}
	}
	if rec.hasInv {
		inv := rec.inv
		if err := cleanInventory(&inv); err != nil {
			fail(err.Error())
			return
		}
		reg.Inventory = &inv
	}
	if old, err := s.store.GetRegistration(ctx, rec.serial); err == nil && old.ClaimedBy == "" {
		if _, edit := clientObjectAccess(p, old.ClientScope); !edit {
			fail("this serial is already pre-registered by another account")
			return
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail("could not check existing registrations")
		return
	}
	var changes []string
	if reg.Name != "" {
		changes = append(changes, "name "+reg.Name)
	}
	if reg.Channel != "" {
		changes = append(changes, "channel "+reg.Channel)
	}
	for _, k := range slices.Sorted(maps.Keys(reg.Labels)) {
		changes = append(changes, k+"="+reg.Labels[k])
	}
	if reg.Inventory != nil {
		changes = append(changes, inventoryChanges(api.Inventory{}, *reg.Inventory)...)
	}
	row.Action, row.Changes = "registered", changes
	plan.regs = append(plan.regs, reg)
}

func (s *Server) handleListRegistrations(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListRegistrations(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.Registration{}
	for _, reg := range all {
		if see, _ := clientObjectAccess(p, reg.ClientScope); see {
			out = append(out, reg)
		}
	}
	// Waiting ones first, newest first.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].ClaimedBy == "") != (out[j].ClaimedBy == "") {
			return out[i].ClaimedBy == ""
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteRegistration(w http.ResponseWriter, r *http.Request) {
	reg, err := s.store.GetRegistration(r.Context(), r.PathValue("serial"))
	p := principalFrom(r.Context())
	if err == nil {
		if see, edit := clientObjectAccess(p, reg.ClientScope); !see {
			err = store.ErrNotFound
		} else if !edit {
			writeError(w, http.StatusForbidden, "you can only remove your own client's pre-registrations")
			return
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no pre-registration for that serial")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.store.DeleteRegistration(r.Context(), reg.Serial); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "registration.deleted", "", "", "pre-registration for serial "+reg.Serial+" removed")
	w.WriteHeader(http.StatusNoContent)
}
