package agent

import (
	"context"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/nasi-lemak/armada/internal/api"
)

// Bluetooth SIG UUIDs for the Device Information Service's Firmware Revision String.
const firmwareRevisionUUID = "00002a26-0000-1000-8000-00805f9b34fb"

// bluezDiscover scans for BLE devices with BlueZ (the Linux Bluetooth stack)
// over the system D-Bus. The agent must be allowed to talk to org.bluez
// (root is by default).
func bluezDiscover(ctx context.Context, d DiscoveryConfig) ([]api.PeripheralReport, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	adapterPath := dbus.ObjectPath("/org/bluez/" + d.Adapter)
	adapter := conn.Object("org.bluez", adapterPath)
	_ = adapter.CallWithContext(ctx, "org.bluez.Adapter1.SetDiscoveryFilter", 0,
		map[string]dbus.Variant{"Transport": dbus.MakeVariant("le"), "DuplicateData": dbus.MakeVariant(false)}).Err
	if err := adapter.CallWithContext(ctx, "org.bluez.Adapter1.StartDiscovery", 0).Err; err != nil && !strings.Contains(err.Error(), "InProgress") {
		return nil, err
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Duration(d.ScanSeconds) * time.Second):
	}
	_ = adapter.Call("org.bluez.Adapter1.StopDiscovery", 0).Err

	objs, err := managedObjects(ctx, conn)
	if err != nil {
		return nil, err
	}
	var out []api.PeripheralReport
	for path, ifaces := range objs {
		props, ok := ifaces["org.bluez.Device1"]
		if !ok || !strings.HasPrefix(string(path), string(adapterPath)+"/") {
			continue
		}
		dev := bleDevice{
			Address: variantString(props["Address"]),
			Name:    variantString(props["Name"]),
			UUIDs:   variantStrings(props["UUIDs"]),
		}
		if dev.Name == "" {
			dev.Name = variantString(props["Alias"])
		}
		if v, ok := props["RSSI"].Value().(int16); ok {
			dev.RSSI = int(v)
		}
		if md, ok := props["ManufacturerData"].Value().(map[uint16]dbus.Variant); ok {
			for id := range md {
				dev.ManufacturerIDs = append(dev.ManufacturerIDs, int(id))
			}
		}
		if !matchBLE(d.Match, dev) {
			continue
		}
		r := api.PeripheralReport{Address: dev.Address, Name: dev.Name, Model: d.Model, Protocol: d.Protocol, RSSI: dev.RSSI}
		if bat, ok := ifaces["org.bluez.Battery1"]; ok {
			if pct, ok := bat["Percentage"].Value().(byte); ok {
				r.BatteryPercent = int(pct)
			}
		}
		if d.ReadFirmwareVersion {
			r.FirmwareVersion = readFirmwareRevision(ctx, conn, path)
		}
		out = append(out, r)
	}
	return out, nil
}

type managed = map[dbus.ObjectPath]map[string]map[string]dbus.Variant

func managedObjects(ctx context.Context, conn *dbus.Conn) (managed, error) {
	var objs managed
	err := conn.Object("org.bluez", "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objs)
	return objs, err
}

// readFirmwareRevision connects to the device and reads the standard
// Firmware Revision String characteristic. It returns "" if unavailable.
func readFirmwareRevision(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dev := conn.Object("org.bluez", path)
	if err := dev.CallWithContext(ctx, "org.bluez.Device1.Connect", 0).Err; err != nil {
		return ""
	}
	defer dev.Call("org.bluez.Device1.Disconnect", 0)
	for ctx.Err() == nil {
		if v, err := dev.GetProperty("org.bluez.Device1.ServicesResolved"); err == nil {
			if ok, _ := v.Value().(bool); ok {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	objs, err := managedObjects(ctx, conn)
	if err != nil {
		return ""
	}
	for p, ifaces := range objs {
		ch, ok := ifaces["org.bluez.GattCharacteristic1"]
		if !ok || !strings.HasPrefix(string(p), string(path)+"/") || !strings.EqualFold(variantString(ch["UUID"]), firmwareRevisionUUID) {
			continue
		}
		var val []byte
		if err := conn.Object("org.bluez", p).CallWithContext(ctx, "org.bluez.GattCharacteristic1.ReadValue", 0, map[string]dbus.Variant{}).Store(&val); err != nil {
			return ""
		}
		return strings.TrimRight(strings.TrimSpace(string(val)), "\x00")
	}
	return ""
}

func variantString(v dbus.Variant) string {
	s, _ := v.Value().(string)
	return s
}

func variantStrings(v dbus.Variant) []string {
	s, _ := v.Value().([]string)
	return s
}
