package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

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

// smartHealth reads one drive's SMART data. name labels the result; path
// is what smartctl opens (/dev/sda, /dev/disk0, /dev/pd0…).
func smartHealth(ctx context.Context, smartctl, name, path string) (api.CheckResult, bool) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := runCommand(cctx, []string{smartctl, "--json=c", "-H", "-A", "-i", path}, nil)
	if err != nil {
		return api.CheckResult{}, false
	}
	return parseSmart(name, []byte(out))
}

// checkStorageScan finds drives with `smartctl --scan-open` (macOS, Windows,
// BSD) and reports SMART health for each.
func checkStorageScan(ctx context.Context, cfg *HealthConfig) []api.CheckResult {
	smartctl, err := exec.LookPath(cfg.Smartctl)
	if err != nil {
		return []api.CheckResult{{Name: "storage smart", Kind: "storage", Status: api.HealthUnknown,
			Message: "install smartmontools (smartctl) to monitor SSD/HDD health"}}
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	out, _, err := runCommand(cctx, []string{smartctl, "--scan-open", "--json=c"}, nil)
	cancel()
	if err != nil {
		return nil
	}
	var scan struct {
		Devices []struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if json.Unmarshal([]byte(out), &scan) != nil {
		return nil
	}
	var res []api.CheckResult
	for _, d := range scan.Devices {
		name := strings.TrimPrefix(d.Name, "/dev/")
		if contains(cfg.IgnoreDevices, name) {
			continue
		}
		if r, ok := smartHealth(ctx, smartctl, name, d.Name); ok {
			res = append(res, r)
		}
	}
	return res
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
