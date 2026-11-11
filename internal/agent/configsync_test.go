package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/server"
)

func TestConfigProfilesEndToEnd(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "myapp")
	reloads := filepath.Join(t.TempDir(), "reloads")
	cfg := &Config{
		EnrollmentToken: h.token(),
		StateDir:        t.TempDir(),
		ManagedConfig: map[string]ManagedConfigTarget{"myapp": {Dir: dir, ReloadCommand: []string{"/bin/sh", "-c",
			`echo reload >> ` + reloads + ` && ! grep -q BROKEN "$ARMADA_CONFIG_DIR/app.conf"`}}},
	}
	a := newTestAgent(t, h, cfg)
	ctx := context.Background()
	if err := a.Enroll(ctx); err != nil {
		t.Fatal(err)
	}
	checkIn(t, a)
	devID := a.DeviceID()
	h.admin("PATCH", "/api/v1/devices/"+devID, map[string]any{"name": "lobby-1"}, nil)

	files := api.ConfigProfile{Name: "myapp config", Kind: api.ConfigFiles, Target: "myapp", Enabled: true,
		Selector: api.Selector{Labels: map[string]string{"site": "test"}},
		Files: []api.ConfigFile{
			{Name: "app.conf", Content: "name = {{.Device.Name}}\nsite = {{index .Device.Labels \"site\"}}\n", Template: true},
			{Name: "extra.conf", Content: "x = 1\n", Mode: "0600"},
		}}
	var p api.ConfigProfile
	h.admin("POST", "/api/v1/config-profiles", files, &p)
	checkIn(t, a) // receives and applies
	if got := readFile(t, filepath.Join(dir, "app.conf")); got != "name = lobby-1\nsite = test" {
		t.Fatalf("rendered file: %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "extra.conf")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v", fi)
	}
	checkIn(t, a) // reports applied
	var det struct{ Config []server.DeviceConfigView }
	h.admin("GET", "/api/v1/devices/"+devID, nil, &det)
	if len(det.Config) != 1 || det.Config[0].Status != api.ConfigApplied || det.Config[0].ProfileName != "myapp config" {
		t.Fatalf("device config view: %+v", det.Config)
	}
	h.admin("GET", "/api/v1/config-profiles/"+p.ID, nil, &p)
	if p.Compliance[api.ConfigApplied] != 1 {
		t.Fatalf("compliance: %v", p.Compliance)
	}

	// A change the application rejects is rolled back, and extra.conf
	// (dropped from the profile) comes back with it.
	p.Files = []api.ConfigFile{{Name: "app.conf", Content: "BROKEN\n"}}
	h.admin("PUT", "/api/v1/config-profiles/"+p.ID, p, nil)
	checkIn(t, a)
	checkIn(t, a)
	if got := readFile(t, filepath.Join(dir, "app.conf")); got != "name = lobby-1\nsite = test" {
		t.Fatalf("not rolled back: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.conf")); err != nil {
		t.Fatal("removed file not restored")
	}
	h.admin("GET", "/api/v1/devices/"+devID, nil, &det)
	if det.Config[0].Status != api.ConfigFailed || !strings.Contains(det.Config[0].Message, "previous files restored") {
		t.Fatalf("failure not reported: %+v", det.Config)
	}

	// Fixed: applied, and the stale file is removed.
	p.Files = []api.ConfigFile{{Name: "app.conf", Content: "fixed\n"}}
	h.admin("PUT", "/api/v1/config-profiles/"+p.ID, p, nil)
	checkIn(t, a)
	if _, err := os.Stat(filepath.Join(dir, "extra.conf")); !os.IsNotExist(err) {
		t.Fatal("file dropped from profile not removed")
	}

	// Agent settings override, and an unknown target fails cleanly.
	f := false
	h.admin("POST", "/api/v1/config-profiles", api.ConfigProfile{Name: "no auto-update", Kind: api.ConfigAgent, Enabled: true,
		Agent: &api.AgentSettings{AutoUpdate: &f}}, nil)
	h.admin("POST", "/api/v1/config-profiles", api.ConfigProfile{Name: "other", Kind: api.ConfigFiles, Target: "nginx", Enabled: true,
		Files: []api.ConfigFile{{Name: "site.conf", Content: "x"}}}, nil)
	checkIn(t, a)
	if a.autoUpdate() {
		t.Fatal("auto_update override not applied")
	}
	if st := a.state.get().Config["files:nginx"]; st.Status != api.ConfigFailed || !strings.Contains(st.Message, "configured: myapp") {
		t.Fatalf("unknown target: %+v", st)
	}

	// Deleting the profile removes its files.
	h.admin("DELETE", "/api/v1/config-profiles/"+p.ID, nil, nil)
	checkIn(t, a)
	checkIn(t, a)
	if _, err := os.Stat(filepath.Join(dir, "app.conf")); !os.IsNotExist(err) {
		t.Fatal("files of a deleted profile kept")
	}
	if _, ok := a.state.get().Config["files:myapp"]; ok {
		t.Fatal("state kept for deleted profile")
	}
	if n := strings.Count(readFile(t, reloads), "reload"); n < 5 {
		t.Fatalf("reload ran %d times", n)
	}
}
