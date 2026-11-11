// Package agent implements the on-device Armada agent: enrollment,
// heartbeats, signed artifact downloads, package updates with health checks
// and rollback, self-update, and remote actions.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Duration is a time.Duration that marshals as a string like "30s".
type Duration struct{ time.Duration }

// UnmarshalJSON accepts "30s" style strings or integer seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		d.Duration = v
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("duration must be a string like \"30s\" or a number of seconds")
	}
	d.Duration = time.Duration(n) * time.Second
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// PackageConfig describes how to install a package on this device.
//
// Commands are argv arrays (no shell) and receive these environment variables:
//
//	ARMADA_PACKAGE, ARMADA_VERSION, ARMADA_ARTIFACT,
//	ARMADA_PREVIOUS_VERSION, ARMADA_PREVIOUS_ARTIFACT
type PackageConfig struct {
	// InstallCommand installs the downloaded artifact. Required.
	InstallCommand []string `json:"install_command"`
	// HealthCommand verifies the new version works; non-zero exit means unhealthy.
	HealthCommand []string `json:"health_command,omitempty"`
	// HealthRetries is how many times to try the health check (default 3).
	HealthRetries int `json:"health_retries,omitempty"`
	// HealthInterval is the wait between health check attempts (default 5s).
	HealthInterval Duration `json:"health_interval,omitempty"`
	// RollbackCommand restores the previous version after a failed install or health check.
	RollbackCommand []string `json:"rollback_command,omitempty"`
	// VersionCommand prints the installed version. If unset, the agent
	// reports the last version it installed.
	VersionCommand []string `json:"version_command,omitempty"`
}

// Config is the agent configuration file (JSON).
type Config struct {
	ServerURL       string `json:"server_url"`
	EnrollmentToken string `json:"enrollment_token,omitempty"`
	// Name is the device name shown in the dashboard (default: hostname).
	Name     string `json:"name,omitempty"`
	StateDir string `json:"state_dir"`
	// SigningPublicKey pins the server's release-signing key (base64 ed25519).
	// If empty, the key received at enrollment is trusted from then on.
	SigningPublicKey string `json:"signing_public_key,omitempty"`
	// CACert is an optional PEM bundle for a private CA.
	CACert string `json:"ca_cert,omitempty"`

	// AutoUpdate lets the agent install updates the server offers (default true).
	AutoUpdate *bool `json:"auto_update,omitempty"`
	// MaintenanceWindow restricts installs to local-time windows, e.g. "02:00-05:00"
	// or "01:00-03:00,13:00-14:00". Empty means any time.
	MaintenanceWindow string `json:"maintenance_window,omitempty"`

	// AllowReboot permits the "reboot" action (default true).
	AllowReboot *bool `json:"allow_reboot,omitempty"`
	// AllowExec permits the "exec" action, which runs arbitrary commands (default false).
	AllowExec bool `json:"allow_exec,omitempty"`
	// RebootCommand is run for the reboot action (default ["systemctl", "reboot"]).
	RebootCommand []string `json:"reboot_command,omitempty"`
	// CommandTimeout bounds install, health, rollback and exec commands (default 10m).
	CommandTimeout Duration `json:"command_timeout,omitempty"`

	// Packages maps package names to install instructions. The agent always
	// manages its own package ("armada-agent") without configuration.
	Packages map[string]PackageConfig `json:"packages,omitempty"`

	// Health tunes the built-in hardware checks (storage, temperature, power…).
	Health HealthConfig `json:"health,omitempty"`
}

// LoadConfig reads a JSON config file. A missing file yields an empty config
// so flags/environment alone can configure the agent.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(cfg); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		}
	}
	return cfg, nil
}

// Validate fills defaults and checks the configuration.
func (c *Config) Validate() error {
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	if c.ServerURL == "" {
		return errors.New("server_url is required")
	}
	if !strings.HasPrefix(c.ServerURL, "http://") && !strings.HasPrefix(c.ServerURL, "https://") {
		return errors.New("server_url must start with http:// or https://")
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/armada"
	}
	if c.AutoUpdate == nil {
		t := true
		c.AutoUpdate = &t
	}
	if c.AllowReboot == nil {
		t := true
		c.AllowReboot = &t
	}
	if len(c.RebootCommand) == 0 {
		c.RebootCommand = []string{"systemctl", "reboot"}
	}
	if c.CommandTimeout.Duration <= 0 {
		c.CommandTimeout.Duration = 10 * time.Minute
	}
	if _, err := ParseWindows(c.MaintenanceWindow); err != nil {
		return err
	}
	for name, p := range c.Packages {
		if len(p.InstallCommand) == 0 {
			return fmt.Errorf("package %q: install_command is required", name)
		}
		if p.HealthRetries <= 0 {
			p.HealthRetries = 3
		}
		if p.HealthInterval.Duration <= 0 {
			p.HealthInterval.Duration = 5 * time.Second
		}
		c.Packages[name] = p
	}
	return nil
}

// Window is a daily local-time interval in minutes since midnight. End may be
// less than Start for windows that span midnight.
type Window struct{ Start, End int }

// ParseWindows parses "HH:MM-HH:MM[,HH:MM-HH:MM...]".
func ParseWindows(s string) ([]Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []Window
	for _, part := range strings.Split(s, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(part), "-")
		if !ok {
			return nil, fmt.Errorf("maintenance window %q must look like 02:00-05:00", part)
		}
		start, err := parseClock(a)
		if err != nil {
			return nil, err
		}
		end, err := parseClock(b)
		if err != nil {
			return nil, err
		}
		out = append(out, Window{start, end})
	}
	return out, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid time %q in maintenance window (use HH:MM)", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// InWindow reports whether t falls in any window. No windows means always.
func InWindow(ws []Window, t time.Time) bool {
	if len(ws) == 0 {
		return true
	}
	m := t.Hour()*60 + t.Minute()
	for _, w := range ws {
		if w.Start <= w.End {
			if m >= w.Start && m < w.End {
				return true
			}
		} else if m >= w.Start || m < w.End {
			return true
		}
	}
	return false
}
