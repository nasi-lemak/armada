package agent

import (
	"context"
	"syscall"
	"time"
	"unsafe"

	"github.com/nasi-lemak/armada/internal/api"
)

func platformChecks() []builtinCheck {
	return []builtinCheck{
		{"storage", 10 * time.Minute, checkStorageScan},
		{"disk-space", time.Minute, checkDiskSpaceWindows},
	}
}

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetDriveType    = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)

const driveFixed = 3

// checkDiskSpaceWindows reports every fixed drive (C:\, D:\…).
func checkDiskSpaceWindows(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	var out []api.CheckResult
	for l := 'C'; l <= 'Z'; l++ {
		root := string(l) + `:\`
		p, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(p))); t != driveFixed {
			continue
		}
		var avail, total, free uint64
		ok, _, _ := procGetDiskFreeSpEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
		if ok == 0 || total == 0 {
			continue
		}
		out = append(out, diskSpaceResult(cfg, root, total, avail))
	}
	return out
}
