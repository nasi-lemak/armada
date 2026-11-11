//go:build unix

package agent

import (
	"os"
	"syscall"
)

// reexec replaces the current process with the binary at exe, keeping the
// same arguments and environment. It only returns on failure.
func reexec(exe string) error {
	return syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), os.Environ())
}
