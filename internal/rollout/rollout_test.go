package rollout

import (
	"fmt"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.2.0", "1.2.0", 0},
		{"1.2.10", "1.2.9", 1},
		{"1.2", "1.2.1", -1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0-rc1", "1.0.0", -1},
		{"1.0.0-rc2", "1.0.0-rc10", -1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0+build5", "1.0.0", 0},
		{"2024.11.03", "2024.9.30", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := Compare(c.b, c.a); got != -c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestBucketDistribution(t *testing.T) {
	in := 0
	d := &api.Deployment{ID: "dep_x", Strategy: api.Strategy{Stages: []int{10, 100}}}
	for i := 0; i < 10000; i++ {
		if InStage(d, fmt.Sprintf("dev_%d", i)) {
			in++
		}
	}
	if in < 800 || in > 1200 {
		t.Fatalf("10%% stage selected %d of 10000 devices", in)
	}
	// Deterministic.
	if Bucket("a", "b") != Bucket("a", "b") {
		t.Fatal("bucket not deterministic")
	}
}

func TestMatches(t *testing.T) {
	dev := &api.Device{ID: "d1", Channel: "beta", Labels: map[string]string{"site": "sg", "role": "kiosk"}}
	cases := []struct {
		sel  api.Selector
		want bool
	}{
		{api.Selector{}, true},
		{api.Selector{Channel: "beta"}, true},
		{api.Selector{Channel: "stable"}, false},
		{api.Selector{Labels: map[string]string{"site": "sg"}}, true},
		{api.Selector{Labels: map[string]string{"site": "kl"}}, false},
		{api.Selector{Labels: map[string]string{"role": "*"}}, true},
		{api.Selector{Labels: map[string]string{"gpu": "*"}}, false},
		{api.Selector{DeviceIDs: []string{"d2", "d1"}}, true},
		{api.Selector{DeviceIDs: []string{"d2"}}, false},
	}
	for i, c := range cases {
		if got := Matches(c.sel, dev); got != c.want {
			t.Errorf("case %d: Matches(%+v) = %v, want %v", i, c.sel, got, c.want)
		}
	}
}

func full(id, version string) *Candidate {
	return &Candidate{
		Deployment: &api.Deployment{ID: id, Package: "app", Version: version, Status: api.DeploymentRunning, Strategy: api.Strategy{Stages: []int{100}}},
		Release:    &api.Release{ID: "rel_" + id, Package: "app", Version: version},
	}
}

func TestDecideOffersNewest(t *testing.T) {
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.0"}}
	dec := Decide(dev, []*Candidate{full("new", "1.2"), full("old", "1.1")}, false)
	if dec.Offer == nil || dec.Offer.Release.Version != "1.2" {
		t.Fatalf("expected 1.2 offered, got %+v", dec.Offer)
	}
}

func TestDecideFallsBackWhenNotInStage(t *testing.T) {
	// Find a device outside a 1% canary.
	canary := full("canary", "1.2")
	canary.Deployment.Strategy.Stages = []int{1, 100}
	devID := ""
	for i := 0; ; i++ {
		id := fmt.Sprintf("d%d", i)
		if !InStage(canary.Deployment, id) {
			devID = id
			break
		}
	}
	dev := &api.Device{ID: devID, Packages: map[string]string{"app": "1.0"}}
	dec := Decide(dev, []*Candidate{canary, full("old", "1.1")}, false)
	if dec.Offer == nil || dec.Offer.Release.Version != "1.1" {
		t.Fatalf("expected fallback to 1.1, got %+v", dec.Offer)
	}
}

func TestDecideNoDowngrade(t *testing.T) {
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.2"}}
	if dec := Decide(dev, []*Candidate{full("old", "1.1")}, false); dec.Offer != nil {
		t.Fatalf("downgrade offered: %+v", dec.Offer)
	}
	rb := full("rollback", "1.1")
	rb.Deployment.Strategy.AllowDowngrade = true
	if dec := Decide(dev, []*Candidate{rb}, false); dec.Offer == nil {
		t.Fatal("rollback deployment with allow_downgrade not offered")
	}
}

func TestDecideRollbackClaimsPackage(t *testing.T) {
	// After rolling back to 1.1, an older 1.2 deployment must not re-upgrade.
	rb := full("rollback", "1.1")
	rb.Deployment.Strategy.AllowDowngrade = true
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.1"}}
	if dec := Decide(dev, []*Candidate{rb, full("bad", "1.2")}, false); dec.Offer != nil {
		t.Fatalf("expected nothing offered, got %+v", dec.Offer)
	}
}

func TestDecideSkipsFailedAndBusyAndPaused(t *testing.T) {
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.0"}}
	c := full("x", "1.1")
	c.Progress = &api.DeviceUpdate{Status: api.UpdateFailed}
	if dec := Decide(dev, []*Candidate{c, full("older", "1.0.5")}, false); dec.Offer != nil {
		t.Fatalf("failed device should not be offered anything, got %+v", dec.Offer)
	}
	if dec := Decide(dev, []*Candidate{full("x", "1.1")}, true); dec.Offer != nil {
		t.Fatal("busy device offered an update")
	}
	p := full("x", "1.1")
	p.Deployment.Status = api.DeploymentPaused
	if dec := Decide(dev, []*Candidate{p, full("older", "1.0.5")}, false); dec.Offer != nil {
		t.Fatalf("paused deployment should hold the package, got %+v", dec.Offer)
	}
}

func TestDecideConfirmsInstalled(t *testing.T) {
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.1"}}
	c := full("x", "1.1")
	c.Progress = &api.DeviceUpdate{Status: api.UpdateInstalling}
	dec := Decide(dev, []*Candidate{c}, false)
	if len(dec.Confirmed) != 1 || dec.Offer != nil {
		t.Fatalf("expected confirmation only, got %+v", dec)
	}
}

func TestDecideFailureStandsWhenVersionMatches(t *testing.T) {
	dev := &api.Device{ID: "d1", Packages: map[string]string{"app": "1.1"}}
	for _, st := range []string{api.UpdateFailed, api.UpdateRolledBack, api.UpdateSucceeded} {
		c := full("x", "1.1")
		c.Progress = &api.DeviceUpdate{Status: st}
		if dec := Decide(dev, []*Candidate{c, full("older", "1.0")}, false); len(dec.Confirmed) != 0 || dec.Offer != nil {
			t.Errorf("status %s: expected no change, got %+v", st, dec)
		}
	}
}

func TestDecidePlatform(t *testing.T) {
	dev := &api.Device{ID: "d1", Info: api.DeviceInfo{OS: "linux", Arch: "arm64"}, Packages: map[string]string{}}
	c := full("x", "1.1")
	c.Release.Arch = "amd64"
	if dec := Decide(dev, []*Candidate{c}, false); dec.Offer != nil {
		t.Fatal("offered amd64 release to arm64 device")
	}
}

func TestEvaluate(t *testing.T) {
	d := &api.Deployment{Strategy: api.Strategy{Stages: []int{10, 100}, StageDurationSeconds: 60, AutoAdvance: true, MaxFailureRate: 0.2, MinSamples: 5}}
	d.Stats = api.DeploymentStats{Succeeded: 2, Failed: 2}
	if ev := Evaluate(d, 10); ev.Pause || ev.Advance {
		t.Fatalf("too few samples / too early: %+v", ev)
	}
	d.Stats = api.DeploymentStats{Succeeded: 3, Failed: 2}
	if ev := Evaluate(d, 10); !ev.Pause {
		t.Fatalf("40%% failure should pause: %+v", ev)
	}
	d.AcknowledgedFailures = 2
	if ev := Evaluate(d, 10); ev.Pause {
		t.Fatalf("acknowledged failures should not pause: %+v", ev)
	}
	d.Stats = api.DeploymentStats{Succeeded: 10}
	if ev := Evaluate(d, 61); !ev.Advance {
		t.Fatalf("should advance after soak: %+v", ev)
	}
	d.StageIndex = 1
	if ev := Evaluate(d, 1000); ev.Advance {
		t.Fatal("should not advance past last stage")
	}
}

func TestValidateStrategy(t *testing.T) {
	s := api.Strategy{Stages: []int{10, 50}}
	if err := ValidateStrategy(&s); err != nil || s.Stages[len(s.Stages)-1] != 100 {
		t.Fatalf("expected 100 appended: %v %v", s.Stages, err)
	}
	for _, bad := range [][]int{{50, 10}, {0, 100}, {10, 101}} {
		s := api.Strategy{Stages: bad}
		if ValidateStrategy(&s) == nil {
			t.Errorf("stages %v accepted", bad)
		}
	}
}
