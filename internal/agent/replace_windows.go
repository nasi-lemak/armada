package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// replaceExe moves src over dst. Windows won't overwrite or delete a
// running executable, but it will rename one, so dst is moved aside first
// (removed on a later start by cleanupOldExes).
func replaceExe(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		aside := fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
		if err := os.Rename(dst, aside); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			_ = os.Rename(aside, dst)
			return err
		}
		return nil
	}
	return os.Rename(src, dst)
}

// cleanupOldExes removes binaries moved aside by earlier updates; the ones
// still running (none, normally) can't be removed and are left for later.
func cleanupOldExes(exe string) {
	old, _ := filepath.Glob(exe + ".old-*")
	for _, f := range old {
		_ = os.Remove(f)
	}
}
