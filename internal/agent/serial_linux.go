package agent

import (
	"os"
	"strings"
)

// platformSerial reads the DMI product serial (PCs, servers, most x86
// boxes), or the device-tree serial (Raspberry Pi and other ARM boards).
func platformSerial() string {
	for _, p := range []string{"/sys/class/dmi/id/product_serial", "/sys/firmware/devicetree/base/serial-number", "/sys/class/dmi/id/board_serial"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := cleanSerial(string(b)); s != "" {
				return s
			}
		}
	}
	// Older Raspberry Pi kernels put it in /proc/cpuinfo.
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "Serial" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
