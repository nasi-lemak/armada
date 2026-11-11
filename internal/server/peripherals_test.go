package server

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestPeripheralAuthorization(t *testing.T) {
	_, ts := setup(t)
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &tok)
	enroll := func(name string) api.EnrollResponse {
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er
	}
	gwA, gwB := enroll("gateway-a"), enroll("gateway-b")
	// A release named like the model but without OS "ble" is not firmware.
	rel := upload(t, ts, "acme-sensor", "2.0.0")
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID, Strategy: &api.Strategy{Stages: []int{100}}}, nil)
	var hr api.HeartbeatResponse
	call(t, ts, gwA.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
		Peripherals: []api.PeripheralReport{{Address: "aa:bb:cc:dd:ee:01", Model: "acme-sensor", Protocol: "mcumgr", FirmwareVersion: "1.0.0"},
			{Address: "", Model: "acme-sensor"}, {Address: "aa:bb:cc:dd:ee:02", Model: "bad model!"}}}, &hr)
	var devs []api.Device
	call(t, ts, admin, "GET", "/api/v1/devices", nil, &devs)
	if len(devs) != 3 { // two gateways + one valid peripheral
		t.Fatalf("devices: %d", len(devs))
	}
	pid := PeripheralID("AA:BB:CC:DD:EE:01")

	// A generic (OS-less) release is never offered to a peripheral.
	if len(hr.PeripheralUpdates) != 0 {
		t.Fatalf("non-firmware release offered: %+v", hr.PeripheralUpdates)
	}
	// Gateway B cannot report on gateway A's peripheral.
	code := call(t, ts, gwB.DeviceKey, "POST", "/api/v1/agent/updates", api.UpdateReport{DeviceID: pid, DeploymentID: "dep_x", Status: api.UpdateSucceeded}, nil)
	if code != 403 {
		t.Fatalf("foreign gateway report: %d", code)
	}
	// The peripheral moves to gateway B when B starts seeing it.
	call(t, ts, gwB.DeviceKey, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{Info: api.DeviceInfo{OS: "linux"},
		Peripherals: []api.PeripheralReport{{Address: "AA:BB:CC:DD:EE:01", Model: "acme-sensor"}}}, nil)
	var det struct{ Device api.Device }
	call(t, ts, admin, "GET", "/api/v1/devices/"+pid, nil, &det)
	if det.Device.ParentID != gwB.DeviceID {
		t.Fatalf("parent: %s", det.Device.ParentID)
	}
	// Peripherals have no credentials of their own.
	if code := call(t, ts, "peripheral:"+pid, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{}, nil); code != 401 {
		t.Fatalf("peripheral placeholder key accepted: %d", code)
	}
}
