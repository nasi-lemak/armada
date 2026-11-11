package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// sysRoot lets tests point the Linux checks at a fake /sys and /proc.
var sysRoot = "/"

func sysPath(p string) string { return filepath.Join(sysRoot, p) }

func platformChecks() []builtinCheck {
	return []builtinCheck{
		{"storage", 10 * time.Minute, checkStorage},
		{"kernel-io", time.Minute, checkKernelIO},
		{"mounts", time.Minute, checkReadOnlyMounts},
		{"raid", 5 * time.Minute, checkRAID},
		{"disk-space", time.Minute, checkDiskSpace},
		{"memory", time.Minute, checkMemory},
		{"temperature", time.Minute, checkTemperature},
		{"power", time.Minute, checkThrottling},
		{"battery", 5 * time.Minute, checkBattery},
	}
}

// ---- block devices ----

type blockDev struct {
	name string // sda, nvme0n1, mmcblk0
	kind string // ata, nvme, emmc, sd, virtual, other
}

var skipBlock = regexp.MustCompile(`^(loop|ram|zram|dm-|md|sr|nbd|fd|mtdblock|mmcblk\d+boot|mmcblk\d+rpmb)`)

func listBlockDevices(cfg *HealthConfig) []blockDev {
	entries, _ := os.ReadDir(sysPath("sys/block"))
	var out []blockDev
	for _, e := range entries {
		n := e.Name()
		if skipBlock.MatchString(n) || contains(cfg.IgnoreDevices, n) {
			continue
		}
		d := blockDev{name: n, kind: "other"}
		switch {
		case strings.HasPrefix(n, "nvme"):
			d.kind = "nvme"
		case strings.HasPrefix(n, "mmcblk"):
			t := readTrim(sysPath("sys/block/" + n + "/device/type"))
			if t == "MMC" {
				d.kind = "emmc"
			} else {
				d.kind = "sd"
			}
		case strings.HasPrefix(n, "vd"), strings.HasPrefix(n, "xvd"):
			d.kind = "virtual"
		case strings.HasPrefix(n, "sd"), strings.HasPrefix(n, "hd"):
			d.kind = "ata"
		}
		out = append(out, d)
	}
	return out
}

// diskOf maps a partition or device name to its disk: sda1 → sda, nvme0n1p2 → nvme0n1, mmcblk0p1 → mmcblk0.
func diskOf(dev string) string {
	dev = strings.TrimPrefix(dev, "/dev/")
	if m := regexp.MustCompile(`^(nvme\d+n\d+|mmcblk\d+)(p\d+)?$`).FindStringSubmatch(dev); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`^([a-z]+?)(\d+)$`).FindStringSubmatch(dev); m != nil && (strings.HasPrefix(dev, "sd") || strings.HasPrefix(dev, "vd") || strings.HasPrefix(dev, "hd") || strings.HasPrefix(dev, "xvd")) {
		return m[1]
	}
	return dev
}

func checkStorage(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	var out []api.CheckResult
	smartctl, smartErr := exec.LookPath(cfg.Smartctl)
	needSmart := false
	for _, d := range listBlockDevices(cfg) {
		switch d.kind {
		case "emmc":
			out = append(out, emmcHealth(d))
		case "sd":
			out = append(out, sdInfo(d))
		case "ata", "nvme":
			if smartErr != nil {
				needSmart = true
				continue
			}
			if r, ok := smartHealth(ctx, smartctl, d); ok {
				out = append(out, r)
			}
		}
	}
	if needSmart {
		out = append(out, api.CheckResult{Name: "storage smart", Kind: "storage", Status: api.HealthUnknown,
			Message: "install smartmontools (smartctl) to monitor SSD/HDD health"})
	}
	return out
}

// eMMC 5.0+ reports its own wear: life_time is the estimated share of rated
// erase cycles used (0x01 = 0-10% … 0x0A = 90-100%, 0x0B = exceeded), and
// pre_eol_info how much of the reserve blocks are consumed.
func emmcHealth(d blockDev) api.CheckResult {
	base := sysPath("sys/block/"+d.name+"/device") + "/"
	r := api.CheckResult{Name: "storage /dev/" + d.name, Kind: "storage", Details: map[string]string{
		"type": "eMMC", "name": readTrim(base + "name"), "manufacturer_id": readTrim(base + "manfid"), "date": readTrim(base + "date"),
	}}
	lt := strings.Fields(readTrim(base + "life_time"))
	eol := parseHex(readTrim(base + "pre_eol_info"))
	worst := int64(-1)
	for _, f := range lt {
		if v := parseHex(f); v > worst {
			worst = v
		}
	}
	if worst < 0 && eol < 0 {
		r.Status, r.Message = api.HealthUnknown, "eMMC does not report wear (needs eMMC 5.0+)"
		return r
	}
	if worst > 0 {
		used := float64(min(worst, 11) * 10)
		r.Value, r.Unit = float(used), "% life used"
	}
	switch {
	case eol == 3 || worst >= 0x0B:
		r.Status, r.Message = api.HealthCritical, "eMMC is worn out; replace the device storage soon"
	case eol == 2 || worst >= 0x0A:
		r.Status, r.Message = api.HealthCritical, fmt.Sprintf("eMMC wear at 90–100%% of rated life (pre-EOL %d)", eol)
	case worst >= 0x09:
		r.Status, r.Message = api.HealthWarning, "eMMC wear at 80–90% of rated life"
	default:
		r.Status, r.Message = api.HealthOK, fmt.Sprintf("eMMC healthy (about %d%% of rated life used)", max(worst, 1)*10)
	}
	return r
}

// SD cards have no standard wear reporting; failures are caught by the
// kernel I/O error and read-only mount checks. This reports what is known.
func sdInfo(d blockDev) api.CheckResult {
	base := sysPath("sys/block/"+d.name+"/device") + "/"
	sectors, _ := strconv.ParseFloat(readTrim(sysPath("sys/block/"+d.name+"/size")), 64)
	return api.CheckResult{Name: "storage /dev/" + d.name, Kind: "storage", Status: api.HealthOK,
		Message: "SD card (no wear reporting; watching for I/O errors and read-only remounts)",
		Details: map[string]string{"type": "SD", "name": readTrim(base + "name"), "manufacturer_id": readTrim(base + "manfid"),
			"oem_id": readTrim(base + "oemid"), "date": readTrim(base + "date"), "size_gb": fmt.Sprintf("%.1f", sectors*512/1e9)}}
}

// smartJSON is the subset of `smartctl --json` output Armada reads.
type smartJSON struct {
	Smartctl struct {
		ExitStatus int `json:"exit_status"`
	} `json:"smartctl"`
	Device struct {
		Protocol string `json:"protocol"`
	} `json:"device"`
	ModelName    string `json:"model_name"`
	SerialNumber string `json:"serial_number"`
	SmartSupport *struct {
		Available bool `json:"available"`
		Enabled   bool `json:"enabled"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature *struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours float64 `json:"hours"`
	} `json:"power_on_time"`
	ATA *struct {
		Table []struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Value int    `json:"value"`
			Raw   struct {
				Value int64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		CriticalWarning     int   `json:"critical_warning"`
		AvailableSpare      int   `json:"available_spare"`
		AvailableSpareThres int   `json:"available_spare_threshold"`
		PercentageUsed      int   `json:"percentage_used"`
		MediaErrors         int64 `json:"media_errors"`
	} `json:"nvme_smart_health_information_log"`
}

func smartHealth(ctx context.Context, smartctl string, d blockDev) (api.CheckResult, bool) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := runCommand(cctx, []string{smartctl, "--json=c", "-H", "-A", "-i", "/dev/" + d.name}, nil)
	if err != nil {
		return api.CheckResult{}, false
	}
	return parseSmart(d.name, []byte(out))
}

// parseSmart interprets smartctl JSON. It returns ok=false for devices
// without SMART (USB bridges, virtual disks) so they don't add noise.
func parseSmart(name string, data []byte) (api.CheckResult, bool) {
	var s smartJSON
	if err := json.Unmarshal(data, &s); err != nil {
		return api.CheckResult{}, false
	}
	if s.Smartctl.ExitStatus&0x3 != 0 || (s.SmartSupport != nil && !s.SmartSupport.Available) {
		return api.CheckResult{}, false
	}
	r := api.CheckResult{Name: "storage /dev/" + name, Kind: "storage", Status: api.HealthOK, Details: map[string]string{
		"model": s.ModelName, "serial": s.SerialNumber, "protocol": s.Device.Protocol}}
	if s.Temperature != nil && s.Temperature.Current > 0 {
		r.Details["temperature_c"] = fmt.Sprintf("%.0f", s.Temperature.Current)
	}
	if s.PowerOnTime != nil {
		r.Details["power_on_hours"] = fmt.Sprintf("%.0f", s.PowerOnTime.Hours)
	}
	var problems []string
	worst := api.HealthOK
	flag := func(status, msg string) {
		problems = append(problems, msg)
		if api.HealthRank(status) > api.HealthRank(worst) {
			worst = status
		}
	}
	if s.SmartStatus != nil && !s.SmartStatus.Passed {
		flag(api.HealthCritical, "SMART overall health check FAILED")
	}
	if n := s.NVMe; n != nil {
		r.Value, r.Unit = float(float64(n.PercentageUsed)), "% life used"
		if n.CriticalWarning != 0 {
			flag(api.HealthCritical, fmt.Sprintf("NVMe critical warning flags 0x%x", n.CriticalWarning))
		}
		if n.AvailableSpareThres > 0 && n.AvailableSpare <= n.AvailableSpareThres {
			flag(api.HealthCritical, fmt.Sprintf("spare capacity %d%% at or below threshold %d%%", n.AvailableSpare, n.AvailableSpareThres))
		}
		switch {
		case n.PercentageUsed >= 90:
			flag(api.HealthCritical, fmt.Sprintf("%d%% of rated life used", n.PercentageUsed))
		case n.PercentageUsed >= 80:
			flag(api.HealthWarning, fmt.Sprintf("%d%% of rated life used", n.PercentageUsed))
		}
		if n.MediaErrors > 0 {
			flag(api.HealthWarning, fmt.Sprintf("%d media errors", n.MediaErrors))
		}
	}
	if a := s.ATA; a != nil {
		for _, at := range a.Table {
			switch at.ID {
			case 5: // Reallocated_Sector_Ct
				if at.Raw.Value > 100 {
					flag(api.HealthCritical, fmt.Sprintf("%d reallocated sectors", at.Raw.Value))
				} else if at.Raw.Value > 0 {
					flag(api.HealthWarning, fmt.Sprintf("%d reallocated sectors", at.Raw.Value))
				}
			case 197: // Current_Pending_Sector
				if at.Raw.Value > 0 {
					flag(api.HealthWarning, fmt.Sprintf("%d sectors pending reallocation", at.Raw.Value))
				}
			case 198: // Offline_Uncorrectable
				if at.Raw.Value > 0 {
					flag(api.HealthCritical, fmt.Sprintf("%d uncorrectable sectors", at.Raw.Value))
				}
			case 177, 231, 233: // SSD wear / life left (normalized: 100 = new)
				r.Value, r.Unit = float(float64(100-at.Value)), "% life used"
				if at.Value <= 10 {
					flag(api.HealthCritical, fmt.Sprintf("SSD life left %d%% (%s)", at.Value, at.Name))
				} else if at.Value <= 20 {
					flag(api.HealthWarning, fmt.Sprintf("SSD life left %d%% (%s)", at.Value, at.Name))
				}
			}
		}
	}
	r.Status = worst
	if len(problems) > 0 {
		r.Message = strings.Join(problems, "; ")
	} else {
		r.Message = "SMART healthy"
		if r.Value != nil {
			r.Message += fmt.Sprintf(" (%.0f%% of rated life used)", *r.Value)
		}
	}
	return r, true
}

// ---- kernel log: I/O errors ----

var ioErrorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`I/O error,? dev (\w+)`),
	regexp.MustCompile(`Buffer I/O error on dev(?:ice)? (\w+)`),
	regexp.MustCompile(`EXT4-fs error \(device (\w+)\)`),
	regexp.MustCompile(`critical (?:medium|target) error,? dev (\w+)`),
	regexp.MustCompile(`(mmcblk\d+): error`),
	regexp.MustCompile(`F2FS-fs \((\w+)\): .*(?:error|corrupt)`),
	regexp.MustCompile(`BTRFS (?:error|critical) \(device (\w+)\)`),
	regexp.MustCompile(`XFS \((\w+)\): .*(?:I/O error|corruption)`),
}

// kmsgState remembers kernel errors seen so far (since boot).
var kmsg = struct {
	sync.Mutex
	lastSeq   int64
	errors    map[string][]time.Time // disk → timestamps
	samples   map[string]string
	remounted map[string]bool // partitions the kernel remounted read-only after errors
}{lastSeq: -1, errors: map[string][]time.Time{}, samples: map[string]string{}, remounted: map[string]bool{}}

var remountRO = regexp.MustCompile(`\((\w+)\): [Rr]emounting filesystem read-only`)

// scanKernelLog reads new /dev/kmsg records (root only). Each record is
// "prio,seq,usec,flags;message". It uses raw syscalls: through os.File, Go's
// poller would turn the non-blocking read into a wait for new kernel
// messages, hanging the health loop.
func scanKernelLog(path string, now time.Time) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	buf := make([]byte, 8192)
	kmsg.Lock()
	defer kmsg.Unlock()
	for i := 0; i < 100000; i++ {
		n, err := syscall.Read(fd, buf)
		switch {
		case err == syscall.EPIPE: // older records were overwritten; continue from the next one
			continue
		case err == syscall.EINTR:
			continue
		case err != nil || n <= 0: // EAGAIN: no more records
			return nil
		}
		recordKernelLine(string(buf[:n]), now)
	}
	return nil
}

func recordKernelLine(rec string, now time.Time) {
	head, msg, ok := strings.Cut(rec, ";")
	if !ok {
		return
	}
	fields := strings.Split(head, ",")
	if len(fields) >= 2 {
		if seq, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			if seq <= kmsg.lastSeq {
				return
			}
			kmsg.lastSeq = seq
		}
	}
	msg = strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
	if m := remountRO.FindStringSubmatch(msg); m != nil {
		kmsg.remounted[m[1]] = true
	}
	for _, re := range ioErrorPatterns {
		if m := re.FindStringSubmatch(msg); m != nil {
			disk := diskOf(m[1])
			kmsg.errors[disk] = append(kmsg.errors[disk], now)
			kmsg.samples[disk] = truncateStr(msg, 200)
			return
		}
	}
}

func checkKernelIO(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	now := time.Now()
	if err := scanKernelLog(sysPath("dev/kmsg"), now); err != nil {
		return nil // not root, or no kmsg (containers)
	}
	return kernelIOResults(now)
}

func kernelIOResults(now time.Time) []api.CheckResult {
	kmsg.Lock()
	defer kmsg.Unlock()
	var out []api.CheckResult
	disks := make([]string, 0, len(kmsg.errors))
	for d := range kmsg.errors {
		disks = append(disks, d)
	}
	sort.Strings(disks)
	for _, d := range disks {
		var recent []time.Time
		for _, t := range kmsg.errors[d] {
			if now.Sub(t) < 24*time.Hour {
				recent = append(recent, t)
			}
		}
		kmsg.errors[d] = recent
		r := api.CheckResult{Name: "io-errors /dev/" + d, Kind: "storage", Value: float(float64(len(recent))), Unit: "errors/24h"}
		switch {
		case len(recent) >= 20:
			r.Status = api.HealthCritical
		case len(recent) > 0:
			r.Status = api.HealthWarning
		default:
			r.Status, r.Message = api.HealthOK, "no I/O errors in the last 24h"
			out = append(out, r)
			continue
		}
		r.Message = fmt.Sprintf("%d kernel I/O errors in 24h; last: %s", len(recent), kmsg.samples[d])
		out = append(out, r)
	}
	return out
}

// ---- mounts ----

type mount struct{ dev, dir, fstype, opts string }

func readMounts() []mount {
	f, err := os.Open(sysPath("proc/mounts"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []mount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) >= 4 && strings.HasPrefix(p[0], "/dev/") {
			out = append(out, mount{p[0], unescapeMount(p[1]), p[2], p[3]})
		}
	}
	return out
}

func unescapeMount(s string) string { return strings.ReplaceAll(s, `\040`, " ") }

var alwaysReadOnlyFS = map[string]bool{"squashfs": true, "iso9660": true, "erofs": true, "udf": true, "cramfs": true}

// checkReadOnlyMounts flags filesystems that should be writable but are
// read-only: the kernel remounts a filesystem read-only when the storage
// under it fails (common with worn SD cards). A mount counts as "should be
// writable" if the kernel logged remounting it, or /etc/fstab mounts it
// read-write. Filesystems that are read-only by design are left alone.
func checkReadOnlyMounts(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	fstab := readFstab()
	kmsg.Lock()
	remounted := map[string]bool{}
	for k, v := range kmsg.remounted {
		remounted[k] = v
	}
	kmsg.Unlock()
	var out []api.CheckResult
	for _, m := range readMounts() {
		ro := strings.HasPrefix(m.opts, "ro,") || m.opts == "ro"
		if !ro || alwaysReadOnlyFS[m.fstype] || contains(cfg.ReadOnlyMountsOK, m.dir) {
			continue
		}
		why := ""
		switch opts, inFstab := fstab[m.dir]; {
		case remounted[strings.TrimPrefix(m.dev, "/dev/")]:
			why = "the kernel remounted it read-only after errors"
		case inFstab && !hasOpt(opts, "ro"):
			why = "it is configured read-write in /etc/fstab"
		default:
			continue // read-only by design
		}
		out = append(out, api.CheckResult{Name: "readonly " + m.dir, Kind: "storage", Status: api.HealthCritical,
			Message: fmt.Sprintf("%s (%s) is read-only but %s; this usually means the storage is failing", m.dir, m.dev, why),
			Details: map[string]string{"device": m.dev, "fstype": m.fstype}})
	}
	return out
}

// readFstab maps mount points to their configured options.
func readFstab() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(sysPath("etc/fstab"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && !strings.HasPrefix(f[0], "#") {
			out[unescapeMount(f[1])] = f[3]
		}
	}
	return out
}

func hasOpt(opts, o string) bool {
	for _, x := range strings.Split(opts, ",") {
		if x == o {
			return true
		}
	}
	return false
}

func checkDiskSpace(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	var out []api.CheckResult
	seen := map[string]bool{}
	for _, m := range readMounts() {
		if seen[m.dev] || alwaysReadOnlyFS[m.fstype] {
			continue
		}
		seen[m.dev] = true
		var st syscall.Statfs_t
		if syscall.Statfs(filepath.Join(sysRoot, m.dir), &st) != nil || st.Blocks == 0 {
			continue
		}
		used := 100 * float64(uint64(st.Blocks)-uint64(st.Bavail)) / float64(uint64(st.Blocks))
		r := api.CheckResult{Name: "disk-space " + m.dir, Kind: "disk_space", Value: float(round1(used)), Unit: "%",
			Details: map[string]string{"device": m.dev, "free_gb": fmt.Sprintf("%.1f", float64(uint64(st.Bavail)*uint64(st.Bsize))/1e9)}}
		if st.Files > 0 {
			inodes := 100 * float64(uint64(st.Files)-uint64(st.Ffree)) / float64(uint64(st.Files))
			r.Details["inodes_used_percent"] = fmt.Sprintf("%.0f", inodes)
			used = max(used, inodes)
		}
		switch {
		case used >= cfg.DiskCritPercent:
			r.Status = api.HealthCritical
		case used >= cfg.DiskWarnPercent:
			r.Status = api.HealthWarning
		default:
			r.Status = api.HealthOK
		}
		r.Message = fmt.Sprintf("%s is %.0f%% full", m.dir, used)
		out = append(out, r)
	}
	return out
}

func checkMemory(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	var total, avail float64
	f, err := os.Open(sysPath("proc/meminfo"))
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(p[1], 64)
		switch p[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total == 0 {
		return nil
	}
	pct := 100 * avail / total
	r := api.CheckResult{Name: "memory", Kind: "memory", Value: float(round1(pct)), Unit: "% available"}
	switch {
	case pct < 5:
		r.Status = api.HealthCritical
	case pct < 10:
		r.Status = api.HealthWarning
	default:
		r.Status = api.HealthOK
	}
	r.Message = fmt.Sprintf("%.0f%% of memory available", pct)
	return []api.CheckResult{r}
}

func checkRAID(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	b, err := os.ReadFile(sysPath("proc/mdstat"))
	if err != nil {
		return nil
	}
	return parseMdstat(string(b))
}

var mdHeader = regexp.MustCompile(`^(md\d+)\s*:\s*(\w+)\s+(\S+)`)
var mdState = regexp.MustCompile(`\[(\d+)/(\d+)\]\s+\[([U_]+)\]`)

func parseMdstat(s string) []api.CheckResult {
	var out []api.CheckResult
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		m := mdHeader.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		r := api.CheckResult{Name: "raid /dev/" + m[1], Kind: "raid", Status: api.HealthOK, Message: m[1] + " " + m[3] + " healthy"}
		if m[2] != "active" {
			r.Status, r.Message = api.HealthCritical, m[1]+" is "+m[2]
		}
		if i+1 < len(lines) {
			if st := mdState.FindStringSubmatch(lines[i+1]); st != nil && st[1] != st[2] {
				r.Status, r.Message = api.HealthCritical, fmt.Sprintf("%s degraded: %s of %s disks active [%s]", m[1], st[2], st[1], st[3])
			}
		}
		if i+2 < len(lines) && strings.Contains(lines[i+2], "recovery") && r.Status == api.HealthOK {
			r.Status, r.Message = api.HealthWarning, m[1]+" is rebuilding"
		}
		out = append(out, r)
	}
	return out
}

// ---- temperature, power, battery ----

func checkTemperature(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	zones, _ := filepath.Glob(sysPath("sys/class/thermal/thermal_zone*"))
	var out []api.CheckResult
	for _, z := range zones {
		milli, err := strconv.ParseFloat(readTrim(z+"/temp"), 64)
		if err != nil || milli <= 0 {
			continue
		}
		c := milli / 1000
		name := readTrim(z + "/type")
		if name == "" {
			name = filepath.Base(z)
		}
		r := api.CheckResult{Name: "temperature " + name, Kind: "temperature", Value: float(round1(c)), Unit: "°C"}
		switch {
		case c >= cfg.TempCritC:
			r.Status = api.HealthCritical
		case c >= cfg.TempWarnC:
			r.Status = api.HealthWarning
		default:
			r.Status = api.HealthOK
		}
		r.Message = fmt.Sprintf("%s at %.0f°C", name, c)
		out = append(out, r)
	}
	return out
}

// checkThrottling reads the Raspberry Pi firmware's throttle flags, which
// reveal a weak power supply (under-voltage) long before it corrupts the SD card.
func checkThrottling(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	raw := readTrim(sysPath("sys/devices/platform/soc/soc:firmware/get_throttled"))
	if raw == "" {
		if _, err := exec.LookPath("vcgencmd"); err != nil {
			return nil
		}
		out, code, err := runCommand(ctx, []string{"vcgencmd", "get_throttled"}, nil)
		if err != nil || code != 0 {
			return nil
		}
		raw = strings.TrimPrefix(strings.TrimSpace(out), "throttled=")
	}
	v := parseHex(raw)
	if v < 0 {
		return nil
	}
	return []api.CheckResult{throttleResult(v)}
}

func throttleResult(v int64) api.CheckResult {
	r := api.CheckResult{Name: "power", Kind: "power", Details: map[string]string{"throttled": fmt.Sprintf("0x%x", v)}}
	switch {
	case v&0x1 != 0:
		r.Status, r.Message = api.HealthCritical, "under-voltage now: the power supply is too weak (risks SD card corruption)"
	case v&0x4 != 0:
		r.Status, r.Message = api.HealthWarning, "CPU is being throttled now"
	case v&0x10000 != 0:
		r.Status, r.Message = api.HealthWarning, "under-voltage occurred since boot"
	case v&0x40000 != 0:
		r.Status, r.Message = api.HealthWarning, "throttling occurred since boot (heat or power)"
	default:
		r.Status, r.Message = api.HealthOK, "power supply OK"
	}
	return r
}

func checkBattery(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	supplies, _ := filepath.Glob(sysPath("sys/class/power_supply/*"))
	var out []api.CheckResult
	for _, s := range supplies {
		if readTrim(s+"/type") != "Battery" {
			continue
		}
		name := filepath.Base(s)
		capStr := readTrim(s + "/capacity")
		status := readTrim(s + "/status")
		health := readTrim(s + "/health")
		r := api.CheckResult{Name: "battery " + name, Kind: "battery", Status: api.HealthOK, Details: map[string]string{"status": status, "health": health}}
		pct, err := strconv.ParseFloat(capStr, 64)
		if err == nil {
			r.Value, r.Unit = float(pct), "%"
		}
		switch {
		case health != "" && health != "Good" && health != "Unknown":
			r.Status, r.Message = api.HealthCritical, "battery health: "+health
		case err == nil && pct < 10 && status == "Discharging":
			r.Status, r.Message = api.HealthWarning, fmt.Sprintf("battery low (%.0f%%)", pct)
		case err == nil:
			r.Message = fmt.Sprintf("battery %.0f%% (%s)", pct, strings.ToLower(status))
		default:
			r.Message = "battery " + strings.ToLower(status)
		}
		out = append(out, r)
	}
	return out
}

// ---- process monitor ----

func checkProcess(c api.CheckSpec, r *api.CheckResult) {
	entries, err := os.ReadDir(sysPath("proc"))
	if err != nil {
		r.Status, r.Message = api.HealthUnknown, "cannot read /proc"
		return
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm := readTrim(sysPath("proc/" + e.Name() + "/comm"))
		if comm == c.Target {
			n++
			continue
		}
		if cmd, err := os.ReadFile(sysPath("proc/" + e.Name() + "/cmdline")); err == nil && len(cmd) > 0 {
			exe := filepath.Base(strings.SplitN(string(cmd), "\x00", 2)[0])
			if exe == c.Target {
				n++
			}
		}
	}
	r.Value = float(float64(n))
	if n == 0 {
		r.Status, r.Message = api.HealthCritical, c.Target+" is not running"
		return
	}
	r.Status, r.Message = api.HealthOK, fmt.Sprintf("%s running (%d process(es))", c.Target, n)
}

// ---- helpers ----

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func parseHex(s string) int64 {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" {
		return -1
	}
	v, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return -1
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
