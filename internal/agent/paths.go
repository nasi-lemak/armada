package agent

import (
	"os"
	"path/filepath"
	"runtime"
)

// windowsDataDir is where the agent keeps its config and state on Windows:
// %ProgramData%\Armada (usually C:\ProgramData\Armada).
func windowsDataDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "Armada")
}

// DefaultConfigPath is where the agent looks for its config file.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(windowsDataDir(), "agent.json")
	}
	return "/etc/armada/agent.json"
}

// DefaultStateDir is where the agent keeps its identity and state.
func DefaultStateDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(windowsDataDir(), "state")
	}
	return "/var/lib/armada"
}

func defaultRebootCommand() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{"shutdown", "/r", "/t", "0", "/c", "Armada: reboot requested"}
	case "darwin", "freebsd", "openbsd", "netbsd":
		return []string{"shutdown", "-r", "now"}
	}
	return []string{"systemctl", "reboot"}
}
