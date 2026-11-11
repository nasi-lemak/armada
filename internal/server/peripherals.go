package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

// maxPeripheralsPerHeartbeat bounds what one gateway can report at once.
const maxPeripheralsPerHeartbeat = 500

// PeripheralID derives a stable device ID from a peripheral's address, so a
// sensor keeps its identity (and update history) if it moves between gateways.
func PeripheralID(address string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(address))))
	return "per_" + idEncoding.EncodeToString(sum[:10])
}

// handlePeripherals records what a gateway sees and returns the firmware
// updates it should apply.
func (s *Server) handlePeripherals(ctx context.Context, gw *api.Device, reports []api.PeripheralReport) ([]api.UpdateInstruction, error) {
	if len(reports) > maxPeripheralsPerHeartbeat {
		reports = reports[:maxPeripheralsPerHeartbeat]
	}
	now := s.now()
	var out []api.UpdateInstruction
	for _, r := range reports {
		addr := strings.ToUpper(strings.TrimSpace(r.Address))
		if addr == "" || !packageRe.MatchString(r.Model) {
			continue // a model is required: firmware releases are matched by it
		}
		id := PeripheralID(addr)
		dev, err := s.store.GetDevice(ctx, id)
		if err != nil {
			name := strings.TrimSpace(r.Name)
			if name == "" {
				name = r.Model + " " + addr
			}
			labels := map[string]string{}
			for k, v := range gw.Labels {
				labels[k] = v
			}
			dev = &api.Device{ID: id, Name: truncate(name, 128), Channel: gw.Channel, Labels: labels, EnrolledAt: now}
		}
		dev.ParentID = gw.ID
		dev.LastSeenAt = now
		dev.Info = api.DeviceInfo{Hostname: truncate(r.Name, 128), OS: api.PeripheralOS, MachineID: addr,
			Model: r.Model, Protocol: truncate(r.Protocol, 64), AgentVersion: ""}
		dev.Metrics = api.Metrics{RSSI: r.RSSI, BatteryPercent: r.BatteryPercent}
		dev.Packages = map[string]string{}
		if r.FirmwareVersion != "" {
			dev.Packages[r.Model] = truncate(r.FirmwareVersion, 64)
		}
		dev.Checks = peripheralChecks(r)
		created, err := s.store.UpsertPeripheral(ctx, dev)
		if err != nil {
			return nil, err
		}
		if created {
			s.event(ctx, "device.discovered", dev.ID, "", fmt.Sprintf("%s discovered by %s (%s, firmware %s)", dev.Name, gw.Name, r.Model, orDash(r.FirmwareVersion)))
		}
		// Re-read so admin-managed channel and labels are used for targeting.
		if stored, err := s.store.GetDevice(ctx, id); err == nil {
			dev = stored
		}
		s.recordHistory(ctx, dev, now)
		inst, err := s.decideUpdate(ctx, dev, false)
		if err != nil {
			return nil, err
		}
		if inst != nil {
			inst.Peripheral = &api.PeripheralTarget{DeviceID: dev.ID, Address: addr, Model: r.Model, Protocol: r.Protocol, FirmwareVersion: r.FirmwareVersion}
			out = append(out, *inst)
		}
	}
	return out, nil
}

func peripheralChecks(r api.PeripheralReport) []api.CheckResult {
	if r.BatteryPercent <= 0 {
		return []api.CheckResult{}
	}
	c := api.CheckResult{Name: "battery", Kind: "battery", Status: api.HealthOK, Unit: "%",
		Message: fmt.Sprintf("battery %d%%", r.BatteryPercent)}
	v := float64(r.BatteryPercent)
	c.Value = &v
	switch {
	case r.BatteryPercent < 5:
		c.Status = api.HealthCritical
	case r.BatteryPercent < 15:
		c.Status = api.HealthWarning
	}
	return []api.CheckResult{c}
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
