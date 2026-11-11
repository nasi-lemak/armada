package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// PeripheralsConfig makes this agent a gateway for nearby devices such as
// BLE sensors: it discovers them, reports them to the server and flashes
// firmware the server rolls out to them.
type PeripheralsConfig struct {
	// ScanInterval is how often to discover peripherals (default 5m).
	ScanInterval Duration `json:"scan_interval,omitempty"`
	// Discovery lists the ways peripherals are found. Results are merged by address.
	Discovery []DiscoveryConfig `json:"discovery"`
	// Drivers maps a protocol name (as reported by discovery) to the command
	// that flashes firmware with it.
	Drivers map[string]DriverConfig `json:"drivers"`
}

// DiscoveryConfig is one discovery method.
type DiscoveryConfig struct {
	// Type is "bluez" (Linux Bluetooth stack over D-Bus) or "command".
	Type string `json:"type"`

	// bluez: Adapter (default hci0), ScanSeconds (default 10) and Match select
	// devices; Model and Protocol label what matched.
	Adapter     string   `json:"adapter,omitempty"`
	ScanSeconds int      `json:"scan_seconds,omitempty"`
	Match       BLEMatch `json:"match,omitempty"`
	Model       string   `json:"model,omitempty"`
	Protocol    string   `json:"protocol,omitempty"`
	// ReadFirmwareVersion connects to each matched device and reads the
	// standard Device Information Service Firmware Revision (0x2A26).
	ReadFirmwareVersion bool `json:"read_firmware_version,omitempty"`

	// command: Command prints peripherals as JSON (an array or one object per
	// line, see api.PeripheralReport). Use it for vendor gateways, serial
	// dongles, Zigbee/LoRa bridges or anything else.
	Command []string `json:"command,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
}

// BLEMatch selects BLE devices. Every non-empty field must match.
type BLEMatch struct {
	NamePrefix     string   `json:"name_prefix,omitempty"`
	ServiceUUID    string   `json:"service_uuid,omitempty"`
	ManufacturerID *int     `json:"manufacturer_id,omitempty"` // Bluetooth SIG company ID, e.g. 89 for Nordic
	Addresses      []string `json:"addresses,omitempty"`
}

// DriverConfig flashes firmware onto one peripheral.
//
// The command gets: ARMADA_PERIPHERAL_ADDRESS, ARMADA_PERIPHERAL_MODEL,
// ARMADA_PROTOCOL, ARMADA_FIRMWARE (path to the verified image),
// ARMADA_FIRMWARE_VERSION, ARMADA_PREVIOUS_VERSION and ARMADA_ADAPTER.
// Exit 0 means the device accepted the image. The agent then rediscovers the
// device and, if it reports a firmware version, checks it is the new one.
type DriverConfig struct {
	Command []string `json:"command"`
	Timeout Duration `json:"timeout,omitempty"` // default 20m
	// VerifyTimeout is how long to wait for the device to reappear with the
	// new version after flashing (default 3m).
	VerifyTimeout Duration `json:"verify_timeout,omitempty"`
}

// Validate fills defaults.
func (p *PeripheralsConfig) Validate() error {
	if p.ScanInterval.Duration <= 0 {
		p.ScanInterval.Duration = 5 * time.Minute
	}
	for i := range p.Discovery {
		d := &p.Discovery[i]
		switch d.Type {
		case "bluez":
			if d.Adapter == "" {
				d.Adapter = "hci0"
			}
			if d.ScanSeconds <= 0 {
				d.ScanSeconds = 10
			}
			if d.Model == "" {
				return fmt.Errorf("peripherals.discovery[%d]: model is required for bluez discovery", i)
			}
		case "command":
			if len(d.Command) == 0 {
				return fmt.Errorf("peripherals.discovery[%d]: command is required", i)
			}
			if d.Timeout.Duration <= 0 {
				d.Timeout.Duration = 2 * time.Minute
			}
		default:
			return fmt.Errorf("peripherals.discovery[%d]: type must be bluez or command", i)
		}
	}
	for name, d := range p.Drivers {
		if len(d.Command) == 0 {
			return fmt.Errorf("peripherals.drivers.%s: command is required", name)
		}
		if d.Timeout.Duration <= 0 {
			d.Timeout.Duration = 20 * time.Minute
		}
		if d.VerifyTimeout.Duration <= 0 {
			d.VerifyTimeout.Duration = 3 * time.Minute
		}
		p.Drivers[name] = d
	}
	return nil
}

// gateway runs discovery and flashes firmware, one peripheral at a time
// (a Bluetooth radio can only do one DFU reliably).
type gateway struct {
	a   *Agent
	cfg *PeripheralsConfig

	mu       sync.Mutex
	seen     []api.PeripheralReport
	lastScan time.Time
	queue    []api.UpdateInstruction
	active   map[string]bool // peripheral IDs queued or flashing
	wake     chan struct{}

	// discover is replaceable in tests.
	discover func(ctx context.Context) []api.PeripheralReport
}

func newGateway(a *Agent, cfg *PeripheralsConfig) *gateway {
	g := &gateway{a: a, cfg: cfg, active: map[string]bool{}, wake: make(chan struct{}, 1)}
	g.discover = g.runDiscovery
	return g
}

// reports returns the latest scan results for the heartbeat.
func (g *gateway) reports() []api.PeripheralReport {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]api.PeripheralReport(nil), g.seen...)
}

// scanIfDue rediscovers peripherals when the scan interval has passed.
func (g *gateway) scanIfDue(ctx context.Context) {
	g.mu.Lock()
	due := time.Since(g.lastScan) >= g.cfg.ScanInterval.Duration
	g.mu.Unlock()
	if due {
		g.scan(ctx)
	}
}

func (g *gateway) scan(ctx context.Context) []api.PeripheralReport {
	found := g.discover(ctx)
	g.mu.Lock()
	g.seen, g.lastScan = found, time.Now()
	g.mu.Unlock()
	return found
}

// runDiscovery runs every configured method and merges results by address.
func (g *gateway) runDiscovery(ctx context.Context) []api.PeripheralReport {
	byAddr := map[string]api.PeripheralReport{}
	for _, d := range g.cfg.Discovery {
		var found []api.PeripheralReport
		var err error
		switch d.Type {
		case "bluez":
			found, err = bluezDiscover(ctx, d)
		case "command":
			found, err = commandDiscover(ctx, d)
		}
		if err != nil {
			g.a.log.Warn("peripheral discovery failed", "type", d.Type, "err", err)
			continue
		}
		for _, p := range found {
			p.Address = strings.ToUpper(strings.TrimSpace(p.Address))
			if p.Address == "" {
				continue
			}
			if old, ok := byAddr[p.Address]; ok {
				p = mergeReport(old, p)
			}
			byAddr[p.Address] = p
		}
	}
	out := make([]api.PeripheralReport, 0, len(byAddr))
	for _, p := range byAddr {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func mergeReport(a, b api.PeripheralReport) api.PeripheralReport {
	if b.Name == "" {
		b.Name = a.Name
	}
	if b.Model == "" {
		b.Model = a.Model
	}
	if b.Protocol == "" {
		b.Protocol = a.Protocol
	}
	if b.FirmwareVersion == "" {
		b.FirmwareVersion = a.FirmwareVersion
	}
	if b.BatteryPercent == 0 {
		b.BatteryPercent = a.BatteryPercent
	}
	if b.RSSI == 0 {
		b.RSSI = a.RSSI
	}
	return b
}

// commandDiscover runs a discovery command and parses its JSON output.
func commandDiscover(ctx context.Context, d DiscoveryConfig) ([]api.PeripheralReport, error) {
	cctx, cancel := context.WithTimeout(ctx, d.Timeout.Duration)
	defer cancel()
	out, code, err := runCommand(cctx, d.Command, nil)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("discovery command: %s", describeRun(out, code, err))
	}
	return parseReports(out, d)
}

func parseReports(out string, d DiscoveryConfig) ([]api.PeripheralReport, error) {
	out = strings.TrimSpace(out)
	var reps []api.PeripheralReport
	if strings.HasPrefix(out, "[") {
		if err := json.Unmarshal([]byte(out), &reps); err != nil {
			return nil, fmt.Errorf("discovery output: %w", err)
		}
	} else {
		sc := bufio.NewScanner(strings.NewReader(out))
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || !strings.HasPrefix(line, "{") {
				continue // tolerate log lines
			}
			var r api.PeripheralReport
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				return nil, fmt.Errorf("discovery output line %q: %w", truncateStr(line, 80), err)
			}
			reps = append(reps, r)
		}
	}
	for i := range reps {
		if reps[i].Model == "" {
			reps[i].Model = d.Model
		}
		if reps[i].Protocol == "" {
			reps[i].Protocol = d.Protocol
		}
	}
	return reps, nil
}

// enqueue adds firmware updates offered by the server, skipping peripherals
// already queued or being flashed.
func (g *gateway) enqueue(insts []api.UpdateInstruction) {
	g.mu.Lock()
	added := false
	for _, u := range insts {
		if u.Peripheral == nil || g.active[u.Peripheral.DeviceID] {
			continue
		}
		g.active[u.Peripheral.DeviceID] = true
		g.queue = append(g.queue, u)
		added = true
	}
	g.mu.Unlock()
	if added {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

// work flashes queued peripherals one at a time until ctx ends.
func (g *gateway) work(ctx context.Context) {
	for {
		if !g.flashNext(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
			case <-time.After(30 * time.Second):
			}
		}
	}
}

// flashNext processes one queued update; it returns false if the queue was empty.
func (g *gateway) flashNext(ctx context.Context) bool {
	g.mu.Lock()
	if len(g.queue) == 0 {
		g.mu.Unlock()
		return false
	}
	u := g.queue[0]
	g.queue = g.queue[1:]
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.active, u.Peripheral.DeviceID)
		g.mu.Unlock()
	}()
	g.flash(ctx, u)
	return true
}

func (g *gateway) flash(ctx context.Context, u api.UpdateInstruction) {
	p := u.Peripheral
	log := g.a.log.With("peripheral", p.Address, "model", p.Model, "version", u.Version)
	rep := func(status, msg string) {
		if status == api.UpdateFailed {
			log.Warn("peripheral update "+status, "msg", msg)
		} else {
			log.Info("peripheral update "+status, "msg", msg)
		}
		g.a.report(ctx, api.UpdateReport{DeviceID: p.DeviceID, DeploymentID: u.DeploymentID, ReleaseID: u.ReleaseID, Status: status, Message: msg})
	}
	drv, ok := g.cfg.Drivers[p.Protocol]
	if !ok {
		rep(api.UpdateFailed, fmt.Sprintf("no firmware driver configured for protocol %q on this gateway", p.Protocol))
		return
	}
	rep(api.UpdateDownloading, "")
	path, err := g.a.fetchArtifact(ctx, u)
	if err != nil {
		rep(api.UpdateFailed, err.Error())
		return
	}
	dir := filepath.Join(g.a.cfg.StateDir, "firmware")
	_ = os.MkdirAll(dir, 0o700)
	image := filepath.Join(dir, safeFileName(p.Model)+"-"+safeFileName(u.Version)+".bin")
	if err := os.Rename(path, image); err != nil {
		rep(api.UpdateFailed, err.Error())
		return
	}
	defer os.Remove(image)
	rep(api.UpdateInstalling, "flashing over "+p.Protocol)
	adapter := ""
	for _, d := range g.cfg.Discovery {
		if d.Adapter != "" {
			adapter = d.Adapter
			break
		}
	}
	env := []string{
		"ARMADA_PERIPHERAL_ADDRESS=" + p.Address,
		"ARMADA_PERIPHERAL_MODEL=" + p.Model,
		"ARMADA_PROTOCOL=" + p.Protocol,
		"ARMADA_FIRMWARE=" + image,
		"ARMADA_FIRMWARE_VERSION=" + u.Version,
		"ARMADA_PREVIOUS_VERSION=" + p.FirmwareVersion,
		"ARMADA_ADAPTER=" + adapter,
	}
	cctx, cancel := context.WithTimeout(ctx, drv.Timeout.Duration)
	out, code, err := runCommand(cctx, drv.Command, env)
	cancel()
	if err != nil || code != 0 {
		rep(api.UpdateFailed, "firmware driver failed: "+describeRun(out, code, err))
		return
	}
	// Confirm the device came back running the new firmware.
	deadline := time.Now().Add(drv.VerifyTimeout.Duration)
	last := ""
	for {
		for _, r := range g.scan(ctx) {
			if r.Address != p.Address {
				continue
			}
			last = r.FirmwareVersion
			if last == "" || last == u.Version {
				msg := "flashed " + u.Version
				if last == "" {
					msg += " (the device does not report its version; trusting the driver)"
				}
				rep(api.UpdateSucceeded, msg)
				return
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(verifyPoll):
		}
	}
	if last == "" {
		rep(api.UpdateFailed, "device did not reappear after flashing")
		return
	}
	rep(api.UpdateFailed, fmt.Sprintf("device reports firmware %s after flashing %s (the bootloader may have rolled back)", last, u.Version))
}

// verifyPoll is the wait between rediscovery attempts after flashing.
var verifyPoll = 10 * time.Second

var errNoBluez = errors.New("BlueZ discovery is only available on Linux")

// bleDevice is what discovery knows about an advertising BLE device.
type bleDevice struct {
	Address         string
	Name            string
	RSSI            int
	UUIDs           []string
	ManufacturerIDs []int
}

// matchBLE reports whether a device satisfies every set field of m.
func matchBLE(m BLEMatch, d bleDevice) bool {
	if m.NamePrefix != "" && !strings.HasPrefix(d.Name, m.NamePrefix) {
		return false
	}
	if m.ServiceUUID != "" {
		ok := false
		for _, u := range d.UUIDs {
			if strings.EqualFold(normalizeUUID(u), normalizeUUID(m.ServiceUUID)) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if m.ManufacturerID != nil {
		ok := false
		for _, id := range d.ManufacturerIDs {
			if id == *m.ManufacturerID {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.Addresses) > 0 {
		ok := false
		for _, a := range m.Addresses {
			if strings.EqualFold(a, d.Address) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// normalizeUUID expands 16-bit UUIDs ("180a") to the full Bluetooth base form.
func normalizeUUID(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	if len(u) == 4 {
		return "0000" + u + "-0000-1000-8000-00805f9b34fb"
	}
	return u
}

// ScanPeripherals discovers peripherals now (used by -once and tests).
func (a *Agent) ScanPeripherals(ctx context.Context) {
	if a.gw != nil {
		a.gw.scan(ctx)
	}
}

// FlashPeripherals applies all queued peripheral firmware updates now.
func (a *Agent) FlashPeripherals(ctx context.Context) {
	if a.gw != nil {
		for a.gw.flashNext(ctx) {
		}
	}
}
