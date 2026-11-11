//go:build !linux

package agent

import "github.com/nasi-lemak/armada/internal/api"

func kernelVersion() string { return "" }

func machineID() string { return "" }

// platformMetrics is a no-op on platforms without a /proc filesystem; only
// the portable fields set by collectMetrics are reported.
func platformMetrics(m *api.Metrics, diskPath string) {}
