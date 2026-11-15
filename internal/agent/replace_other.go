//go:build !windows

package agent

import "os"

// replaceExe moves src over dst; on Unix a running binary can be replaced.
func replaceExe(src, dst string) error { return os.Rename(src, dst) }

func cleanupOldExes(string) {}
