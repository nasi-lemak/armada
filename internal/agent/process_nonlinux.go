//go:build !linux

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

func checkProcess(c api.CheckSpec, r *api.CheckResult) {
	out, code, err := runCommand(context.Background(), processLister(c.Target), nil)
	if err != nil {
		r.Status, r.Message = api.HealthUnknown, "cannot list processes: "+err.Error()
		return
	}
	if code != 0 || !strings.Contains(strings.ToLower(out), strings.ToLower(c.Target)) {
		r.Status, r.Message = api.HealthCritical, c.Target+" is not running"
		return
	}
	r.Status, r.Message = api.HealthOK, fmt.Sprintf("%s running", c.Target)
}
