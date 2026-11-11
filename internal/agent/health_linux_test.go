package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// fakeRoot builds a fake filesystem root for the /sys and /proc readers.
func fakeRoot(t *testing.T, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := sysRoot
	sysRoot = root
	t.Cleanup(func() { sysRoot = old })
}

func TestParseSmart(t *testing.T) {
	cases := []struct {
		name, json, status, msg string
		skip                    bool
	}{
		{"nvme healthy", `{"smartctl":{"exit_status":0},"device":{"protocol":"NVMe"},"model_name":"Samsung SSD 980","smart_status":{"passed":true},
			"nvme_smart_health_information_log":{"critical_warning":0,"available_spare":100,"available_spare_threshold":10,"percentage_used":3,"media_errors":0},
			"temperature":{"current":41},"power_on_time":{"hours":1234}}`, api.HealthOK, "3% of rated life used", false},
		{"nvme worn out", `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},
			"nvme_smart_health_information_log":{"critical_warning":0,"available_spare":100,"available_spare_threshold":10,"percentage_used":92,"media_errors":0}}`,
			api.HealthCritical, "92% of rated life used", false},
		{"nvme spare low", `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},
			"nvme_smart_health_information_log":{"critical_warning":1,"available_spare":5,"available_spare_threshold":10,"percentage_used":40}}`,
			api.HealthCritical, "spare capacity", false},
		{"hdd reallocating", `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"ata_smart_attributes":{"table":[
			{"id":5,"name":"Reallocated_Sector_Ct","value":100,"raw":{"value":3}},
			{"id":197,"name":"Current_Pending_Sector","value":100,"raw":{"value":1}}]}}`, api.HealthWarning, "3 reallocated sectors", false},
		{"smart failed", `{"smartctl":{"exit_status":8},"smart_status":{"passed":false}}`, api.HealthCritical, "FAILED", false},
		{"ssd life low", `{"smartctl":{"exit_status":0},"smart_status":{"passed":true},"ata_smart_attributes":{"table":[
			{"id":177,"name":"Wear_Leveling_Count","value":15,"raw":{"value":2100}}]}}`, api.HealthWarning, "life left 15%", false},
		{"usb bridge without smart", `{"smartctl":{"exit_status":2}}`, "", "", true},
		{"smart unsupported", `{"smartctl":{"exit_status":4},"smart_support":{"available":false}}`, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := parseSmart("sda", []byte(c.json))
			if c.skip {
				if ok {
					t.Fatalf("expected device to be skipped, got %+v", r)
				}
				return
			}
			if !ok || r.Status != c.status || !strings.Contains(r.Message, c.msg) {
				t.Fatalf("got ok=%v %s %q, want %s containing %q", ok, r.Status, r.Message, c.status, c.msg)
			}
		})
	}
}

func TestStorageEMMCAndSD(t *testing.T) {
	fakeRoot(t, map[string]string{
		"sys/block/mmcblk0/device/type":         "MMC\n",
		"sys/block/mmcblk0/device/name":         "8GTF4R\n",
		"sys/block/mmcblk0/device/life_time":    "0x0a 0x02\n",
		"sys/block/mmcblk0/device/pre_eol_info": "0x01\n",
		"sys/block/mmcblk1/device/type":         "SD\n",
		"sys/block/mmcblk1/device/name":         "SC32G\n",
		"sys/block/mmcblk1/size":                "62333952\n",
		"sys/block/mmcblk0boot0/device/type":    "MMC\n",
		"sys/block/loop0/size":                  "0\n",
	})
	res := checkStorage(context.Background(), &HealthConfig{Smartctl: "definitely-not-installed"})
	byName := map[string]api.CheckResult{}
	for _, r := range res {
		byName[r.Name] = r
	}
	if r := byName["storage /dev/mmcblk0"]; r.Status != api.HealthCritical || *r.Value != 100 {
		t.Fatalf("eMMC at 90-100%% life: %+v", r)
	}
	if r := byName["storage /dev/mmcblk1"]; r.Status != api.HealthOK || r.Details["size_gb"] != "31.9" {
		t.Fatalf("SD card: %+v", r)
	}
	if len(res) != 2 {
		t.Fatalf("boot partitions and loop devices should be skipped: %+v", res)
	}
	fakeRoot(t, map[string]string{
		"sys/block/mmcblk0/device/type":         "MMC\n",
		"sys/block/mmcblk0/device/life_time":    "0x02 0x01\n",
		"sys/block/mmcblk0/device/pre_eol_info": "0x01\n",
		"sys/block/sda/size":                    "100\n",
	})
	res = checkStorage(context.Background(), &HealthConfig{Smartctl: "definitely-not-installed"})
	if res[0].Status != api.HealthOK || !strings.Contains(res[0].Message, "20%") {
		t.Fatalf("healthy eMMC: %+v", res[0])
	}
	if last := res[len(res)-1]; last.Name != "storage smart" || last.Status != api.HealthUnknown {
		t.Fatalf("missing smartctl should be reported once for SATA disks: %+v", res)
	}
}

func TestKernelIOErrors(t *testing.T) {
	kmsg.Lock()
	kmsg.lastSeq, kmsg.errors, kmsg.samples = -1, map[string][]time.Time{}, map[string]string{}
	kmsg.Unlock()
	now := time.Now()
	for i, line := range []string{
		"3,100,5000,-;blk_update_request: I/O error, dev mmcblk0, sector 123456 op 0x1:(WRITE)",
		"3,101,5001,-;EXT4-fs error (device mmcblk0p2): ext4_find_entry:1455: inode #2: comm sh: reading directory lblock 0",
		"3,102,5002,-;Buffer I/O error on dev sda1, logical block 0, async page read",
		"6,103,5003,-;usb 1-1: new high-speed USB device number 2",
		"3,101,5001,-;EXT4-fs error (device mmcblk0p2): duplicate record must be ignored",
	} {
		kmsg.Lock()
		recordKernelLine(line, now.Add(time.Duration(i)*time.Second))
		kmsg.Unlock()
	}
	res := kernelIOResults(now.Add(time.Minute))
	got := map[string]api.CheckResult{}
	for _, r := range res {
		got[r.Name] = r
	}
	if r := got["io-errors /dev/mmcblk0"]; r.Status != api.HealthWarning || *r.Value != 2 {
		t.Fatalf("mmcblk0: %+v", r)
	}
	if r := got["io-errors /dev/sda"]; r.Status != api.HealthWarning || *r.Value != 1 {
		t.Fatalf("sda: %+v", r)
	}
	// Errors age out after 24 hours.
	for _, r := range kernelIOResults(now.Add(25 * time.Hour)) {
		if r.Status != api.HealthOK {
			t.Fatalf("old errors still flagged: %+v", r)
		}
	}
}

func TestDiskOf(t *testing.T) {
	for in, want := range map[string]string{"sda1": "sda", "sda": "sda", "nvme0n1p3": "nvme0n1", "mmcblk0p2": "mmcblk0", "mmcblk1": "mmcblk1", "vdb12": "vdb", "/dev/sdb2": "sdb"} {
		if got := diskOf(in); got != want {
			t.Errorf("diskOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMountsRAIDPowerTemperatureBattery(t *testing.T) {
	fakeRoot(t, map[string]string{
		"proc/mounts": "/dev/mmcblk0p2 / ext4 ro,relatime 0 0\n/dev/mmcblk0p1 /boot vfat rw,relatime 0 0\n" +
			"/dev/loop0 /snap/core squashfs ro,nodev 0 0\n/dev/sda1 /data\\040disk ext4 ro,noatime 0 0\n/dev/sdb1 /media ext4 ro 0 0\n" +
			"/dev/sdc1 /logs ext4 ro 0 0\n/dev/vdc /opt/tools ext4 ro 0 0\nproc /proc proc rw 0 0\n",
		"etc/fstab":                                           "# root is read-only by design\n/dev/mmcblk0p2 / ext4 ro,noatime 0 1\n/dev/sda1 /data\\040disk ext4 defaults,noatime 0 2\n/dev/sdb1 /media ext4 ro 0 2\n",
		"proc/mdstat":                                         "Personalities : [raid1]\nmd0 : active raid1 sdb1[1] sda1[0]\n      976630464 blocks super 1.2 [2/1] [U_]\n\nmd1 : active raid1 sdc1[1] sdd1[0]\n      1000 blocks [2/2] [UU]\n",
		"sys/class/thermal/thermal_zone0/type":                "cpu-thermal\n",
		"sys/class/thermal/thermal_zone0/temp":                "84500\n",
		"sys/class/power_supply/BAT0/type":                    "Battery\n",
		"sys/class/power_supply/BAT0/capacity":                "7\n",
		"sys/class/power_supply/BAT0/status":                  "Discharging\n",
		"sys/class/power_supply/BAT0/health":                  "Good\n",
		"sys/class/power_supply/AC/type":                      "Mains\n",
		"sys/devices/platform/soc/soc:firmware/get_throttled": "0x50005\n",
	})
	cfg := &HealthConfig{}
	cfg.defaults()
	kmsg.Lock()
	kmsg.lastSeq = -1
	recordKernelLine("2,900,1,-;EXT4-fs (sdc1): Remounting filesystem read-only", time.Now())
	kmsg.Unlock()
	ro := checkReadOnlyMounts(context.Background(), cfg)
	names := []string{}
	for _, r := range ro {
		names = append(names, r.Name)
	}
	// /data is rw in fstab and /logs was remounted by the kernel; /, /media (ro in
	// fstab), squashfs and /opt/tools (not in fstab, no kernel message) are fine.
	if strings.Join(names, ",") != "readonly /data disk,readonly /logs" {
		t.Fatalf("read-only mounts: %v", names)
	}
	raid := checkRAID(context.Background(), cfg)
	if len(raid) != 2 || raid[0].Status != api.HealthCritical || raid[1].Status != api.HealthOK {
		t.Fatalf("raid: %+v", raid)
	}
	temp := checkTemperature(context.Background(), cfg)
	if len(temp) != 1 || temp[0].Status != api.HealthWarning || *temp[0].Value != 84.5 {
		t.Fatalf("temperature: %+v", temp)
	}
	bat := checkBattery(context.Background(), cfg)
	if len(bat) != 1 || bat[0].Status != api.HealthWarning {
		t.Fatalf("battery: %+v", bat)
	}
	pw := checkThrottling(context.Background(), cfg)
	if len(pw) != 1 || pw[0].Status != api.HealthCritical || !strings.Contains(pw[0].Message, "under-voltage") {
		t.Fatalf("power: %+v", pw)
	}
	if r := throttleResult(0x50000); r.Status != api.HealthWarning {
		t.Fatalf("past under-voltage: %+v", r)
	}
	if r := throttleResult(0); r.Status != api.HealthOK {
		t.Fatalf("no throttling: %+v", r)
	}
}

func TestProcessCheck(t *testing.T) {
	self := readTrim("/proc/self/comm")
	var r api.CheckResult
	checkProcess(api.CheckSpec{Target: self}, &r)
	if r.Status != api.HealthOK {
		t.Fatalf("own process %q not found: %+v", self, r)
	}
	checkProcess(api.CheckSpec{Target: "no-such-process-xyz"}, &r)
	if r.Status != api.HealthCritical {
		t.Fatalf("missing process: %+v", r)
	}
}
