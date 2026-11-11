//go:build !unix

package agent

import "os"

// reexec exits so the service manager restarts the agent with the new binary.
func reexec(exe string) error {
	os.Exit(0)
	return nil
}
