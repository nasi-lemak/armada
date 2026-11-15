//go:build !unix

package agent

import "os"

// reexec exits so the service manager restarts the agent with the new
// binary. On Windows the service is installed with restart-on-failure
// recovery actions, and a service process that ends without reporting
// "stopped" counts as failed, so the Service Control Manager starts it again.
func reexec(exe string) error {
	os.Exit(3)
	return nil
}
