package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestMatchBLE(t *testing.T) {
	nordic := 89
	d := bleDevice{Address: "C0:98:E5:49:00:01", Name: "Acme-Sensor-01", UUIDs: []string{"0000180a-0000-1000-8000-00805f9b34fb", "8d53dc1d-1db7-4cd3-868b-8a527460aa84"}, ManufacturerIDs: []int{89}}
	cases := []struct {
		m    BLEMatch
		want bool
	}{
		{BLEMatch{}, true},
		{BLEMatch{NamePrefix: "Acme-"}, true},
		{BLEMatch{NamePrefix: "Other"}, false},
		{BLEMatch{ServiceUUID: "180A"}, true}, // 16-bit form
		{BLEMatch{ServiceUUID: "8D53DC1D-1DB7-4CD3-868B-8A527460AA84"}, true},
		{BLEMatch{ServiceUUID: "180f"}, false},
		{BLEMatch{ManufacturerID: &nordic}, true},
		{BLEMatch{Addresses: []string{"c0:98:e5:49:00:01"}}, true},
		{BLEMatch{NamePrefix: "Acme-", Addresses: []string{"AA:BB:CC:DD:EE:FF"}}, false},
	}
	for i, c := range cases {
		if got := matchBLE(c.m, d); got != c.want {
			t.Errorf("case %d: matchBLE(%+v) = %v", i, c.m, got)
		}
	}
}

func TestParseReports(t *testing.T) {
	d := DiscoveryConfig{Model: "acme-sensor", Protocol: "mcumgr"}
	lines := "scanning hci0...\n{\"address\":\"aa:bb\",\"firmware_version\":\"1.0.0\"}\n{\"address\":\"cc:dd\",\"model\":\"other\",\"protocol\":\"esp-ota\"}\n"
	r, err := parseReports(lines, d)
	if err != nil || len(r) != 2 || r[0].Model != "acme-sensor" || r[0].Protocol != "mcumgr" || r[1].Model != "other" {
		t.Fatalf("lines: %+v %v", r, err)
	}
	r, err = parseReports(`[{"address":"ee:ff","battery_percent":80}]`, d)
	if err != nil || len(r) != 1 || r[0].BatteryPercent != 80 {
		t.Fatalf("array: %+v %v", r, err)
	}
	if _, err := parseReports("{not json}", d); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

// fakeFleet is a JSON-lines file standing in for the radio: discovery prints
// it and the "flash" driver rewrites a device's firmware_version.
func fakeFleet(t *testing.T, devices map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "radio.jsonl")
	var b strings.Builder
	for addr, line := range devices {
		_ = addr
		b.WriteString(line + "\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
	return path
}

func TestEndToEndPeripheralFirmware(t *testing.T) {
	h := newHarness(t)
	verifyPoll = 10 * time.Millisecond
	radio := fakeFleet(t, map[string]string{
		"1": `{"address":"C0:98:E5:00:00:01","name":"Acme Sensor 1","firmware_version":"1.0.0","battery_percent":90,"rssi":-60}`,
		"2": `{"address":"C0:98:E5:00:00:02","name":"Acme Sensor 2","firmware_version":"1.0.0","battery_percent":12}`,
		"3": `{"address":"C0:98:E5:00:00:03","name":"Acme Sensor 3 (bad bootloader)","firmware_version":"1.0.0"}`,
		"4": `{"address":"24:0A:C4:00:00:04","name":"Other thing","model":"other-sensor","protocol":"esp-ota","firmware_version":"0.9"}`,
	})
	log := filepath.Join(t.TempDir(), "flash.log")
	// The driver checks it got the right image, then "flashes" by rewriting the
	// version — except sensor 3, whose bootloader rolls the update back.
	driver := `set -e
grep -q "acme firmware 1.1.0" "$ARMADA_FIRMWARE"
echo "$ARMADA_PERIPHERAL_ADDRESS $ARMADA_PREVIOUS_VERSION->$ARMADA_FIRMWARE_VERSION $ARMADA_PROTOCOL" >> ` + log + `
case "$ARMADA_PERIPHERAL_ADDRESS" in *:03) exit 0;; esac
sed -i "/$ARMADA_PERIPHERAL_ADDRESS/s/\"firmware_version\":\"[^\"]*\"/\"firmware_version\":\"$ARMADA_FIRMWARE_VERSION\"/" ` + radio
	cfg := &Config{
		EnrollmentToken: h.token(), StateDir: t.TempDir(), Health: HealthConfig{Disabled: true},
		Peripherals: &PeripheralsConfig{
			Discovery: []DiscoveryConfig{{Type: "command", Command: []string{"cat", radio}, Model: "acme-sensor", Protocol: "mcumgr"}},
			Drivers:   map[string]DriverConfig{"mcumgr": {Command: []string{"/bin/sh", "-c", driver}, VerifyTimeout: Duration{100 * time.Millisecond}}},
		},
	}
	a := newTestAgent(t, h, cfg)
	ctx := context.Background()
	if err := a.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	a.ScanPeripherals(ctx)
	checkIn(t, a)

	var gw struct {
		Device      api.Device
		Peripherals []api.Device
	}
	h.admin("GET", "/api/v1/devices/"+a.DeviceID(), nil, &gw)
	if len(gw.Peripherals) != 4 {
		t.Fatalf("expected 4 peripherals, got %d", len(gw.Peripherals))
	}
	byAddr := map[string]api.Device{}
	for _, p := range gw.Peripherals {
		byAddr[p.Info.MachineID] = p
		if p.ParentID != a.DeviceID() || p.Labels["site"] != "test" || p.Status != api.DeviceOnline {
			t.Fatalf("peripheral not linked to gateway: %+v", p)
		}
	}
	if p := byAddr["C0:98:E5:00:00:02"]; p.Health != api.HealthWarning || p.Packages["acme-sensor"] != "1.0.0" {
		t.Fatalf("low-battery sensor: %+v", p)
	}

	// Roll out firmware 1.1.0 to every acme-sensor. A Linux package rollout
	// and the other model must not reach these devices.
	fw := h.uploadOS("acme-sensor", "1.1.0", api.PeripheralOS, []byte("acme firmware 1.1.0"))
	var dep api.Deployment
	h.admin("POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: fw.ID}, &dep)
	h.deploy(h.upload("app", "2.0.0", []byte("linux app")))

	checkIn(t, a)           // receives the peripheral updates
	a.FlashPeripherals(ctx) // flashes them one by one
	checkIn(t, a)

	flashed, _ := os.ReadFile(log)
	if n := strings.Count(string(flashed), "\n"); n != 3 || !strings.Contains(string(flashed), "C0:98:E5:00:00:01 1.0.0->1.1.0 mcumgr") {
		t.Fatalf("flash log:\n%s", flashed)
	}
	var detail struct {
		Deployment api.Deployment
		Updates    []*api.DeviceUpdate
	}
	h.admin("GET", "/api/v1/deployments/"+dep.ID, nil, &detail)
	status := map[string]string{}
	for _, u := range detail.Updates {
		status[u.DeviceID] = u.Status + ": " + u.Message
	}
	if len(status) != 3 {
		t.Fatalf("updates: %v", status)
	}
	for addr, want := range map[string]string{"C0:98:E5:00:00:01": "succeeded", "C0:98:E5:00:00:02": "succeeded", "C0:98:E5:00:00:03": "failed"} {
		got := status[byAddr[addr].ID]
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s: %s, want %s", addr, got, want)
		}
	}
	if !strings.Contains(status[byAddr["C0:98:E5:00:00:03"].ID], "rolled back") {
		t.Errorf("rollback not explained: %s", status[byAddr["C0:98:E5:00:00:03"].ID])
	}
	h.admin("GET", "/api/v1/devices/"+byAddr["C0:98:E5:00:00:01"].ID, nil, &gw)
	if gw.Device.Packages["acme-sensor"] != "1.1.0" {
		t.Fatalf("server did not see new firmware: %+v", gw.Device.Packages)
	}
}
