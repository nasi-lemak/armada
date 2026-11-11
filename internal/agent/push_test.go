package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// With a 30s heartbeat, a queued action still runs within a few seconds
// because the server wakes the agent over its long-poll.
func TestPushRunsActionsImmediately(t *testing.T) {
	h := newHarness(t)
	logDir := t.TempDir()
	var lines []string
	for i := 1; i <= 500; i++ {
		lvl := "INFO"
		if i%100 == 0 {
			lvl = "ERROR"
		}
		lines = append(lines, lvl+" line "+strconv.Itoa(i))
	}
	os.WriteFile(filepath.Join(logDir, "app.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	cfg := &Config{EnrollmentToken: h.token(), StateDir: t.TempDir(), LogFiles: map[string]string{"app": filepath.Join(logDir, "app.log")}}
	a := newTestAgent(t, h, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	var dev api.Device
	waitFor(t, 10*time.Second, "agent connected", func() bool {
		var devs []api.Device
		h.admin("GET", "/api/v1/devices", nil, &devs)
		if len(devs) == 1 {
			dev = devs[0]
		}
		return len(devs) == 1 && devs[0].Connected
	})

	run := func(args ...string) api.Action {
		start := time.Now()
		var act api.Action
		h.admin("POST", "/api/v1/devices/"+dev.ID+"/actions", api.CreateActionRequest{Type: api.ActionLogs, Args: args}, &act)
		waitFor(t, 8*time.Second, "logs action result", func() bool {
			var acts []api.Action
			h.admin("GET", "/api/v1/devices/"+dev.ID+"/actions", nil, &acts)
			for _, x := range acts {
				if x.ID == act.ID && x.Status != api.ActionQueued && x.Status != api.ActionSent {
					act = x
					return true
				}
			}
			return false
		})
		if el := time.Since(start); el > 8*time.Second {
			t.Fatalf("action took %s; push did not wake the agent", el)
		}
		return act
	}

	act := run("file", "name=app", "lines=3", "grep=error")
	if act.Status != api.ActionSucceeded {
		t.Fatalf("logs: %+v", act)
	}
	got := strings.Split(act.Output, "\n")
	if len(got) != 4 || got[1] != "ERROR line 300" || got[3] != "ERROR line 500" {
		t.Fatalf("filtered logs:\n%s", act.Output)
	}
	act = run("file", "name=secrets")
	if act.Status != api.ActionFailed || !strings.Contains(act.Output, "configured: app") {
		t.Fatalf("unlisted file: %+v", act)
	}

	// Waking without an action still triggers a prompt check-in.
	before := time.Now()
	var wr api.WakeResponse
	h.admin("POST", "/api/v1/devices/"+dev.ID+"/wake", nil, &wr)
	if !wr.Connected {
		t.Fatal("wake: device not connected")
	}
	waitFor(t, 8*time.Second, "check-in after wake", func() bool {
		var d struct{ Device api.Device }
		h.admin("GET", "/api/v1/devices/"+dev.ID, nil, &d)
		return d.Device.LastSeenAt.After(before)
	})
}

func TestLogArgsAndTail(t *testing.T) {
	if _, err := parseLogArgs([]string{"journal", "bogus=1"}); err == nil {
		t.Fatal("unknown option accepted")
	}
	r, err := parseLogArgs([]string{"journal", "unit=nginx.service", "lines=9000", "since=2h"})
	if err != nil || r.lines != 5000 || r.since != 2*time.Hour || r.unit != "nginx.service" {
		t.Fatalf("parse: %+v %v", r, err)
	}
	p := filepath.Join(t.TempDir(), "big.log")
	os.WriteFile(p, []byte("first line that gets cut\nsecond\nthird\n"), 0o644)
	b, err := tailFile(p, 12) // starts mid-"second": the partial line is dropped
	if err != nil || string(b) != "third\n" {
		t.Fatalf("tail: %q %v", b, err)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
