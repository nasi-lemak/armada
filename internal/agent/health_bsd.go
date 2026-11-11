//go:build !linux && !darwin && !windows

package agent

import "time"

// Other platforms (BSDs…) get SMART storage checks via smartctl.
func platformChecks() []builtinCheck {
	return []builtinCheck{{"storage", 10 * time.Minute, checkStorageScan}}
}
