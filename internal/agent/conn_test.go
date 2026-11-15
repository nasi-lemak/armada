package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestConnTracker(t *testing.T) {
	var c connTracker
	if c.report() != nil {
		t.Fatal("report before any failure")
	}
	c.failed(fmt.Errorf("heartbeat: %w", &HTTPError{Status: 503}))
	if c.report() != nil {
		t.Fatal("an HTTP error means the server was reached")
	}
	c.failed(errors.New("dial tcp: lookup fleet.example.com: no such host"))
	c.failed(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))
	r := c.report()
	if r == nil || r.FailedAttempts != 2 || r.LastError != "dial tcp 10.0.0.1:443: i/o timeout" || r.Since == nil {
		t.Fatalf("report: %+v", r)
	}
	c.reached()
	if c.report() != nil {
		t.Fatal("report after reaching the server")
	}
}
