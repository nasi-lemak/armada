package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/nasi-lemak/armada/internal/api"
)

func kernelVersion() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return strings.TrimSpace(string(b))
}

func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func platformMetrics(m *api.Metrics, diskPath string) {
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			up, _ := strconv.ParseFloat(f[0], 64)
			m.UptimeSeconds = int64(up)
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			m.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				m.MemTotalBytes = kb * 1024
			case "MemAvailable:":
				m.MemAvailableBytes = kb * 1024
			}
		}
		f.Close()
	}
	var st syscall.Statfs_t
	if syscall.Statfs(diskPath, &st) == nil {
		m.DiskTotalBytes = uint64(st.Blocks) * uint64(st.Bsize)
		m.DiskFreeBytes = uint64(st.Bavail) * uint64(st.Bsize)
		m.DiskUsedBytes = (uint64(st.Blocks) - uint64(st.Bfree)) * uint64(st.Bsize)
	}
}
