package server

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestConfigProfileValidation(t *testing.T) {
	_, ts := setup(t)
	win := "02:00-04:00"
	for name, bad := range map[string]api.ConfigProfile{
		"no name":         {Kind: api.ConfigAgent, Agent: &api.AgentSettings{MaintenanceWindow: &win}},
		"bad kind":        {Name: "x", Kind: "registry"},
		"path in name":    {Name: "x", Kind: api.ConfigFiles, Target: "app", Files: []api.ConfigFile{{Name: "../etc/passwd", Content: "x"}}},
		"bad target":      {Name: "x", Kind: api.ConfigFiles, Target: "/etc", Files: []api.ConfigFile{{Name: "a", Content: "x"}}},
		"bad mode":        {Name: "x", Kind: api.ConfigFiles, Target: "app", Files: []api.ConfigFile{{Name: "a", Content: "x", Mode: "999"}}},
		"bad template":    {Name: "x", Kind: api.ConfigFiles, Target: "app", Files: []api.ConfigFile{{Name: "a", Content: "{{.Device.Name", Template: true}}},
		"bad package":     {Name: "x", Kind: api.ConfigAndroidApp, Target: "kiosk", Values: map[string]any{"a": "b"}},
		"nested value":    {Name: "x", Kind: api.ConfigAndroidApp, Target: "com.example.kiosk", Values: map[string]any{"a": map[string]any{"b": 1}}},
		"windowed no win": {Name: "x", Kind: api.ConfigAndroidSystemUpdate, SystemUpdate: &api.SystemUpdatePolicy{Type: "windowed"}},
		"empty agent":     {Name: "x", Kind: api.ConfigAgent, Agent: &api.AgentSettings{}},
	} {
		if code := call(t, ts, admin, "POST", "/api/v1/config-profiles", bad, nil); code != 400 {
			t.Errorf("%s accepted: %d", name, code)
		}
	}
	ok := api.ConfigProfile{Name: "kiosk app", Kind: api.ConfigAndroidApp, Target: "com.example.kiosk", Enabled: true,
		Values: map[string]any{"server": "https://x", "debug": false, "slots": 4.0, "langs": []any{"en", "ms"}},
		Files:  []api.ConfigFile{{Name: "dropped", Content: "kind fields only"}}}
	var p api.ConfigProfile
	if code := call(t, ts, admin, "POST", "/api/v1/config-profiles", ok, &p); code != 201 || p.Files != nil {
		t.Fatalf("create: %d %+v", code, p)
	}
	if code := call(t, ts, admin, "POST", "/api/v1/config-profiles", ok, nil); code != 409 {
		t.Fatalf("duplicate name: %d", code)
	}
}

func TestWinningProfiles(t *testing.T) {
	now := time.Now()
	linux := &api.Device{ID: "d1", Labels: map[string]string{"site": "sg"}, Info: api.DeviceInfo{OS: "linux"}}
	android := &api.Device{ID: "d2", Info: api.DeviceInfo{OS: "android"}}
	base := &api.ConfigProfile{ID: "a", Kind: api.ConfigFiles, Target: "app", Enabled: true, UpdatedAt: now}
	site := &api.ConfigProfile{ID: "b", Kind: api.ConfigFiles, Target: "app", Enabled: true, Priority: 10, UpdatedAt: now.Add(-time.Hour),
		Selector: api.Selector{Labels: map[string]string{"site": "sg"}}}
	off := &api.ConfigProfile{ID: "c", Kind: api.ConfigFiles, Target: "app", Priority: 99, UpdatedAt: now}
	upd := &api.ConfigProfile{ID: "d", Kind: api.ConfigAndroidSystemUpdate, Enabled: true, UpdatedAt: now}
	all := []*api.ConfigProfile{base, site, off, upd}
	if w := winningProfiles(all, linux); len(w) != 1 || w["files:app"].ID != "b" {
		t.Fatalf("linux: %+v", w)
	}
	if w := winningProfiles(all, android); len(w) != 1 || w[api.ConfigAndroidSystemUpdate] == nil {
		t.Fatalf("android: %+v", w)
	}
}
