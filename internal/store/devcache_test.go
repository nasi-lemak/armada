package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// fill sets every field reachable from v to a non-zero value.
func fill(v reflect.Value, n *int) {
	*n++
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("s%d", *n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*n))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(*n) + 0.5)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), n)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := 0; i < 2; i++ {
			fill(s.Index(i), n)
		}
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		for i := 0; i < 2; i++ {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			fill(k, n)
			fill(e, n)
			m.SetMapIndex(k, e)
		}
		v.Set(m)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(time.Unix(int64(*n), 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), n)
			}
		}
	}
}

// shared reports memory that a and b both refer to.
func shared(a, b reflect.Value, path string) []string {
	var out []string
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		return shared(a.Elem(), b.Elem(), path)
	case reflect.Slice:
		if a.Len() == 0 {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		for i := 0; i < a.Len(); i++ {
			out = append(out, shared(a.Index(i), b.Index(i), fmt.Sprintf("%s[%d]", path, i))...)
		}
	case reflect.Map:
		if a.Len() == 0 {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			return []string{path}
		}
		for _, k := range a.MapKeys() {
			out = append(out, shared(a.MapIndex(k), b.MapIndex(k), fmt.Sprintf("%s[%v]", path, k))...)
		}
	case reflect.Struct:
		if a.Type() == reflect.TypeOf(time.Time{}) {
			return nil // a value; its *Location is shared and never changed
		}
		for i := 0; i < a.NumField(); i++ {
			if a.Type().Field(i).IsExported() {
				out = append(out, shared(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
			}
		}
	}
	return out
}

func TestCloneDeviceIsDeep(t *testing.T) {
	var d api.Device
	n := 0
	fill(reflect.ValueOf(&d).Elem(), &n)
	c := cloneDevice(&d)
	if !reflect.DeepEqual(&d, c) {
		t.Fatalf("clone differs:\n%+v\n%+v", d, *c)
	}
	if s := shared(reflect.ValueOf(&d), reflect.ValueOf(c), "Device"); len(s) > 0 {
		t.Errorf("cloneDevice shares %v with the original; copy them in cloneDevice", s)
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "armada.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func enrollTest(t *testing.T, s *Store, id, name string) {
	t.Helper()
	ctx := context.Background()
	tok := &api.EnrollmentToken{ID: "tok-" + id, Labels: map[string]string{"customer": "acme"}}
	if err := s.CreateToken(ctx, tok, "hash-"+id); err != nil {
		t.Fatal(err)
	}
	d := &api.Device{ID: id, Name: name, Channel: "stable", Labels: map[string]string{}, Packages: map[string]string{},
		Info: api.DeviceInfo{OS: "linux"}, EnrolledAt: time.Now(), LastSeenAt: time.Now(), Lifecycle: api.LifecycleActive}
	if err := s.Enroll(ctx, "hash-"+id, d, "key-"+id, "", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// checkCache compares what the cache answers with the database.
func checkCache(t *testing.T, s *Store, step string) {
	t.Helper()
	ctx := context.Background()
	got, err := s.ListAllDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.listDevices(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name, id`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after %s the cache differs from the database:\ncache: %s\ndb:    %s", step, mustJSON(got), mustJSON(want))
	}
}

func TestDeviceCacheFollowsWrites(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	enrollTest(t, s, "dev-b", "kiosk-b")
	checkCache(t, s, "enroll")
	enrollTest(t, s, "dev-a", "kiosk-a")
	checkCache(t, s, "second enroll")
	v := 41.5
	now := time.Now()
	steps := []struct {
		name string
		do   func() error
	}{
		{"heartbeat", func() error {
			return s.RecordHeartbeat(ctx, "dev-a", api.DeviceInfo{OS: "linux", Arch: "arm64", IPs: []string{"10.0.0.2"}},
				api.Metrics{NumCPU: 4, UptimeSeconds: 99}, map[string]string{"kiosk": "2.0.0"}, "update deferred", now)
		}},
		{"heartbeat without packages", func() error {
			return s.RecordHeartbeat(ctx, "dev-b", api.DeviceInfo{OS: "linux"}, api.Metrics{}, nil, "", now.Add(time.Second))
		}},
		{"health", func() error {
			return s.SetDeviceHealth(ctx, "dev-a", []api.CheckResult{{Name: "disk /", Kind: "disk_space", Status: api.HealthWarning, Value: &v, Details: map[string]string{"mount": "/"}, CheckedAt: now}})
		}},
		{"health cleared", func() error { return s.SetDeviceHealth(ctx, "dev-b", nil) }},
		{"license", func() error { return s.SetLicenseRevoked(ctx, "dev-a", true) }},
		{"inventory", func() error {
			return s.SetDeviceInventory(ctx, "dev-a", &api.Inventory{AssetTag: "A-1", Location: "till 3"})
		}},
		{"inventory cleared", func() error { return s.SetDeviceInventory(ctx, "dev-a", &api.Inventory{}) }},
		{"rename (reorders)", func() error {
			return s.UpdateDeviceMeta(ctx, "dev-a", "zz-kiosk", "beta", map[string]string{"customer": "globex"})
		}},
		{"lifecycle", func() error {
			d, err := s.GetDevice(ctx, "dev-b")
			if err != nil {
				return err
			}
			d.Lifecycle, d.LifecycleInfo = api.LifecycleRepair, &api.LifecycleInfo{Note: "screen", ChangedAt: now}
			return s.SetDeviceLifecycle(ctx, d)
		}},
		{"new peripheral", func() error {
			_, err := s.UpsertPeripheral(ctx, &api.Device{ID: "ble-1", Name: "sensor", Channel: "stable", Labels: map[string]string{},
				Info: api.DeviceInfo{OS: api.PeripheralOS}, ParentID: "dev-a", EnrolledAt: now, LastSeenAt: now})
			return err
		}},
		{"peripheral seen again", func() error {
			_, err := s.UpsertPeripheral(ctx, &api.Device{ID: "ble-1", Info: api.DeviceInfo{OS: api.PeripheralOS, Model: "v2"},
				Packages: map[string]string{"fw": "1.1"}, ParentID: "dev-b", LastSeenAt: now.Add(time.Minute),
				Checks: []api.CheckResult{{Name: "battery", Status: api.HealthCritical}}})
			return err
		}},
		{"delete", func() error { return s.DeleteDevice(ctx, "dev-b") }},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		checkCache(t, s, st.name)
	}
	if p, _ := s.ListPeripherals(ctx, "dev-b"); len(p) != 1 || p[0].ID != "ble-1" {
		t.Errorf("peripherals of dev-b: %v", p)
	}
	if d, _ := s.ListDevices(ctx); len(d) != 2 {
		t.Errorf("ListDevices: %d devices", len(d))
	}

	// What callers get is theirs to change.
	got, _ := s.ListAllDevices(ctx)
	for _, d := range got {
		d.Name = "changed"
		d.Labels["x"] = "y"
		d.Packages["x"] = "y"
		for i := range d.Checks {
			if d.Checks[i].Value != nil {
				*d.Checks[i].Value = -1
			}
		}
	}
	checkCache(t, s, "changing returned devices")

	// A failed write leaves the cache alone.
	if err := s.RecordHeartbeat(ctx, "dev-gone", api.DeviceInfo{}, api.Metrics{}, nil, "", now); err == nil {
		t.Error("heartbeat for a missing device succeeded")
	}
	checkCache(t, s, "failed heartbeat")
}

// TestDeviceCacheConcurrent writes and reads from many goroutines (run it
// with -race), then checks the cache still matches the database.
func TestDeviceCacheConcurrent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		enrollTest(t, s, fmt.Sprintf("dev-%02d", i), fmt.Sprintf("kiosk-%02d", i))
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 60; i++ {
				id := fmt.Sprintf("dev-%02d", (w*7+i)%20)
				switch i % 6 {
				case 0, 1, 2:
					s.RecordHeartbeat(ctx, id, api.DeviceInfo{OS: "linux", Hostname: fmt.Sprint(w, i)}, api.Metrics{Load1: float64(i)},
						map[string]string{"kiosk": fmt.Sprint(i)}, "", time.Now())
				case 3:
					s.SetDeviceHealth(ctx, id, []api.CheckResult{{Name: "cpu", Status: api.HealthOK}})
				case 4:
					if w%4 == 0 {
						s.UpdateDeviceMeta(ctx, id, fmt.Sprintf("kiosk-%02d-%d", (w*7+i)%20, i), "stable", map[string]string{"w": fmt.Sprint(w)})
					}
				case 5:
					devs, err := s.ListAllDevices(ctx)
					if err != nil || len(devs) != 20 {
						t.Errorf("list: %d devices, %v", len(devs), err)
						return
					}
					devs[0].Labels["mine"] = "yes"
				}
			}
		}(w)
	}
	wg.Wait()
	checkCache(t, s, "concurrent writes")
}
