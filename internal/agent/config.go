// Package agent implements the on-device Armada agent: enrollment,
// heartbeats, signed artifact downloads, package updates with health checks
// and rollback, self-update, and remote actions.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/rollout"
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
	// RebootRequired is for updates that take effect on reboot, such as A/B
	// OS images (RAUC, SWUpdate, Mender). After install_command the agent
	// reboots; when it starts again it checks version_command reports the new
	// version, runs health_command, then confirm_command. Booting the old
	// version (the bootloader fell back) is reported as rolled back.
	RebootRequired bool `json:"reboot_required,omitempty"`
	// ConfirmCommand marks a rebooted update good, e.g. ["rauc", "status", "mark-good"].
	ConfirmCommand []string `json:"confirm_command,omitempty"`
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
	// AllowLogs permits the "logs" action: reading the systemd journal and
	// the files listed in LogFiles (default true).
	AllowLogs *bool `json:"allow_logs,omitempty"`
	// LogFiles names log files the server may ask for, e.g.
	// {"app": "/var/log/myapp/app.log"}. Only these files can be read.
	LogFiles map[string]string `json:"log_files,omitempty"`
	// DisableDelta always downloads updates in full instead of reusing
	// chunks of the installed version.
	DisableDelta bool `json:"disable_delta,omitempty"`
	// DisablePush turns off the long-poll connection that lets the server
	// reach this device instantly. Actions then wait for the next heartbeat.
	DisablePush bool `json:"disable_push,omitempty"`
	// RebootCommand is run for the reboot action (default ["systemctl", "reboot"]).
	RebootCommand []string `json:"reboot_command,omitempty"`
	// CommandTimeout bounds install, health, rollback and exec commands (default 10m).
	CommandTimeout Duration `json:"command_timeout,omitempty"`

	// Packages maps package names to install instructions. The agent always
	// manages its own package ("armada-agent") without configuration.
	Packages map[string]PackageConfig `json:"packages,omitempty"`

	// Health tunes the built-in hardware checks (storage, temperature, power…).
	Health HealthConfig `json:"health,omitempty"`

	// Peripherals makes this agent a gateway for nearby devices (BLE sensors…).
	Peripherals *PeripheralsConfig `json:"peripherals,omitempty"`

	// ManagedConfig names directories the server may write configuration
	// files into (config profiles of kind "files" target these names).
	// Nothing outside these directories can be written.
	ManagedConfig map[string]ManagedConfigTarget `json:"managed_config,omitempty"`
	// AllowRemoteSettings lets config profiles override maintenance_window
	// and auto_update (default true).
	AllowRemoteSettings *bool `json:"allow_remote_settings,omitempty"`
}

// ManagedConfigTarget is a directory the server manages files in.
type ManagedConfigTarget struct {
	Dir string `json:"dir"`
	// ReloadCommand runs after files change (e.g. systemctl reload myapp).
	// If it fails, the previous files are restored and it runs again.
	ReloadCommand []string `json:"reload_command,omitempty"`
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
	if c.AllowLogs == nil {
		t := true
		c.AllowLogs = &t
	}
	if c.AllowRemoteSettings == nil {
		t := true
		c.AllowRemoteSettings = &t
	}
	for name, mc := range c.ManagedConfig {
		if !filepath.IsAbs(mc.Dir) || filepath.Clean(mc.Dir) == "/" {
			return fmt.Errorf("managed_config %q: dir must be an absolute path other than /", name)
		}
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
	if c.Peripherals != nil {
		if err := c.Peripherals.Validate(); err != nil {
			return err
		}
	}
	for name, p := range c.Packages {
		if len(p.InstallCommand) == 0 {
			return fmt.Errorf("package %q: install_command is required", name)
		}
		if p.RebootRequired && len(p.VersionCommand) == 0 {
			return fmt.Errorf("package %q: reboot_required needs version_command, to tell which version booted", name)
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
type Window = rollout.Window

// ParseWindows parses "HH:MM-HH:MM[,HH:MM-HH:MM...]".
func ParseWindows(s string) ([]Window, error) { return rollout.ParseWindows(s) }

// InWindow reports whether t falls in any window. No windows means always.
func InWindow(ws []Window, t time.Time) bool { return rollout.InWindow(ws, t) }
