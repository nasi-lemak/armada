package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// platformSerial reads the Mac's serial number from the I/O Registry.
func platformSerial() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ioreg", "-c", "IOPlatformExpertDevice", "-d", "2").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, `"IOPlatformSerialNumber"`) {
			if _, v, ok := strings.Cut(line, "="); ok {
				return strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	return ""
}
