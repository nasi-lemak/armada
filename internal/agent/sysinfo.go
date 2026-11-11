package agent

import (
	"net"
	"os"
	"runtime"
	"sort"

	"github.com/nasi-lemak/armada/internal/api"
)

// collectInfo gathers static device information.
func collectInfo() api.DeviceInfo {
	host, _ := os.Hostname()
	return api.DeviceInfo{
		Hostname:     host,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Kernel:       kernelVersion(),
		MachineID:    machineID(),
		IPs:          localIPs(),
		AgentVersion: Version,
	}
}

func localIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	sort.Strings(out)
	return out
}

// collectMetrics gathers a health snapshot. diskPath selects the filesystem to report.
func collectMetrics(diskPath string) api.Metrics {
	m := api.Metrics{NumCPU: runtime.NumCPU()}
	platformMetrics(&m, diskPath)
	return m
}
