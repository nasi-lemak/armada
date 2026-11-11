package agent

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func platformChecks() []builtinCheck {
	return []builtinCheck{
		{"storage", 10 * time.Minute, checkStorageScan},
		{"disk-space", time.Minute, checkDiskSpaceDarwin},
	}
}

// On macOS the writable data lives on /System/Volumes/Data; / is a sealed system volume.
func checkDiskSpaceDarwin(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	var out []api.CheckResult
	for _, mp := range []string{"/System/Volumes/Data", "/"} {
		if _, err := os.Stat(mp); err != nil {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(mp, &st) != nil || st.Blocks == 0 {
			continue
		}
		bs := uint64(st.Bsize)
		out = append(out, diskSpaceResult(cfg, mp, uint64(st.Blocks)*bs, uint64(st.Bavail)*bs))
		break // the first that exists is the one that fills up
	}
	return out
}
