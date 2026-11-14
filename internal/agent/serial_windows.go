package agent

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// platformSerial reads the BIOS serial number.
func platformSerial() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"(Get-CimInstance Win32_BIOS).SerialNumber").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
