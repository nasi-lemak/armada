// Package api defines the wire types shared by the Armada server, agent and CLI.
package api

import (
	"encoding/json"
	"time"
)

// AgentPackage is the reserved package name for the Armada agent itself.
// Releases of this package are applied by the agent as a self-update.
const AgentPackage = "armada-agent"

// DeviceInfo is static-ish information an agent reports about its host.
type DeviceInfo struct {
	Hostname     string   `json:"hostname"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Kernel       string   `json:"kernel,omitempty"`
	MachineID    string   `json:"machine_id,omitempty"`
	IPs          []string `json:"ips,omitempty"`
	AgentVersion string   `json:"agent_version"`
	// Model and Protocol describe peripherals (hardware model, firmware update method).
	Model    string `json:"model,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// Metrics is a point-in-time health snapshot reported with every heartbeat.
type Metrics struct {
	UptimeSeconds     int64   `json:"uptime_seconds"`
	Load1             float64 `json:"load1"`
	NumCPU            int     `json:"num_cpu"`
	MemTotalBytes     uint64  `json:"mem_total_bytes"`
	MemAvailableBytes uint64  `json:"mem_available_bytes"`
	DiskTotalBytes    uint64  `json:"disk_total_bytes"`
	DiskFreeBytes     uint64  `json:"disk_free_bytes"` // available to unprivileged users
	DiskUsedBytes     uint64  `json:"disk_used_bytes"`
	// Peripherals only.
	RSSI           int `json:"rssi,omitempty"`
	BatteryPercent int `json:"battery_percent,omitempty"`
}

// Device is a managed machine as seen by the control plane.
type Device struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Channel    string            `json:"channel"`
	Labels     map[string]string `json:"labels"`
	Info       DeviceInfo        `json:"info"`
	Metrics    Metrics           `json:"metrics"`
	Packages   map[string]string `json:"packages"`
	Status     string            `json:"status"` // online | offline
	EnrolledAt time.Time         `json:"enrolled_at"`
	LastSeenAt time.Time         `json:"last_seen_at"`
	// Connected means the agent holds a live push connection, so actions
	// reach it within a second or two instead of at the next heartbeat.
	Connected   bool   `json:"connected,omitempty"`
	AgentStatus string `json:"agent_status,omitempty"` // free-form note from the agent, e.g. "update deferred"
	// Attestation is set for devices that proved a hardware-backed key at enrollment.
	Attestation *DeviceAttestation `json:"attestation,omitempty"`
	// LicenseRevoked stops the device from receiving new app licenses.
	LicenseRevoked bool `json:"license_revoked"`
	// Health is the worst status among Checks: ok | unknown | warning | critical.
	Health string        `json:"health"`
	Checks []CheckResult `json:"checks,omitempty"`
	// ParentID is set for peripherals (e.g. BLE devices) reached through a gateway device.
	ParentID string `json:"parent_id,omitempty"`
	// Maintenance is set (on read) while a maintenance window covers the device.
	Maintenance *MaintenanceInfo `json:"maintenance,omitempty"`
	// Lifecycle is where the device is in its life: active | spare | repair | retired.
	Lifecycle     string         `json:"lifecycle"`
	LifecycleInfo *LifecycleInfo `json:"lifecycle_info,omitempty"`
}

// Lifecycle states.
//   - active: in service. Gets updates and alerts.
//   - spare: in stock, ready to replace a device. Gets updates (stays current), no alerts.
//   - repair: out of service (RMA, at the bench). No updates, no alerts; it can still check in.
//   - retired: decommissioned for good. Its key is destroyed and its licenses revoked; the
//     record and history are kept. To use the hardware again, enroll it again.
const (
	LifecycleActive  = "active"
	LifecycleSpare   = "spare"
	LifecycleRepair  = "repair"
	LifecycleRetired = "retired"
)

// ValidLifecycle reports whether s is a lifecycle state.
func ValidLifecycle(s string) bool {
	switch s {
	case LifecycleActive, LifecycleSpare, LifecycleRepair, LifecycleRetired:
		return true
	}
	return false
}

// GetsUpdates reports whether rollouts offer the device updates.
func (d *Device) GetsUpdates() bool {
	return d.Lifecycle == "" || d.Lifecycle == LifecycleActive || d.Lifecycle == LifecycleSpare
}

// Alerting reports whether alert rules watch the device.
func (d *Device) Alerting() bool { return d.Lifecycle == "" || d.Lifecycle == LifecycleActive }

// LifecycleInfo records the last lifecycle change and replacement links.
type LifecycleInfo struct {
	Note      string    `json:"note,omitempty"`
	ChangedAt time.Time `json:"changed_at"`
	ChangedBy string    `json:"changed_by,omitempty"`
	// Replaces and ReplacedBy link a device and its replacement (RMA).
	Replaces   string `json:"replaces,omitempty"`
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// LifecycleRequest changes a device's lifecycle state.
type LifecycleRequest struct {
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// ReplaceRequest swaps a device for a replacement: the replacement takes
// over its name, channel and labels; the old device leaves service.
type ReplaceRequest struct {
	ReplacementID string `json:"replacement_id"`
	// OldState is what becomes of the old device: repair (default), spare or retired.
	OldState string `json:"old_state,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Health statuses, from best to worst.
const (
	HealthOK       = "ok"
	HealthUnknown  = "unknown"
	HealthWarning  = "warning"
	HealthCritical = "critical"
)

// HealthRank orders statuses by severity (higher is worse).
func HealthRank(s string) int {
	switch s {
	case HealthOK:
		return 0
	case HealthUnknown:
		return 1
	case HealthWarning:
		return 2
	case HealthCritical:
		return 3
	}
	return 1
}

// WorstHealth returns the most severe status among results ("ok" if none).
func WorstHealth(rs []CheckResult) string {
	worst := HealthOK
	for _, r := range rs {
		if HealthRank(r.Status) > HealthRank(worst) {
			worst = r.Status
		}
	}
	return worst
}

// CheckResult is the latest outcome of one health check on a device.
type CheckResult struct {
	// Name is unique per device, e.g. "storage /dev/sda", "temperature cpu", "monitor myapp".
	Name string `json:"name"`
	// Kind groups checks: storage, temperature, power, battery, disk_space, memory, raid,
	// systemd, process, http, tcp, command.
	Kind      string            `json:"kind"`
	Status    string            `json:"status"`
	Message   string            `json:"message"`
	Value     *float64          `json:"value,omitempty"`
	Unit      string            `json:"unit,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
	CheckedAt time.Time         `json:"checked_at"`
}

// CheckSpec defines a program check that agents run.
type CheckSpec struct {
	// Type is systemd | process | http | tcp | command.
	Type string `json:"type"`
	// Target is the systemd unit, process name, URL or host:port.
	Target string `json:"target,omitempty"`
	// Command (type command) follows the Nagios plugin convention:
	// exit 0 = ok, 1 = warning, 2 = critical, anything else = unknown.
	Command []string `json:"command,omitempty"`
	// ExpectStatus is the HTTP status required (default: any 2xx).
	ExpectStatus int `json:"expect_status,omitempty"`
	// Contains must appear in the HTTP body or command output.
	Contains string `json:"contains,omitempty"`
	// WarnMs flags slow HTTP/TCP responses as warning.
	WarnMs          int `json:"warn_ms,omitempty"`
	IntervalSeconds int `json:"interval_seconds,omitempty"` // default 60
	TimeoutSeconds  int `json:"timeout_seconds,omitempty"`  // default 10
}

// Monitor is a server-defined check run on every device its selector matches.
type Monitor struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Selector  Selector  `json:"selector"`
	Check     CheckSpec `json:"check"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MonitorCheck is a monitor as sent to an agent.
type MonitorCheck struct {
	Name  string    `json:"name"`
	Check CheckSpec `json:"check"`
}

// DeviceAttestation summarises a verified Android key attestation.
type DeviceAttestation struct {
	SecurityLevel string    `json:"security_level"` // tee | strongbox
	BootState     string    `json:"boot_state"`     // verified | self-signed | unverified | failed
	DeviceLocked  bool      `json:"device_locked"`
	OSPatchLevel  int64     `json:"os_patch_level,omitempty"`
	VerifiedAt    time.Time `json:"verified_at"`
}

// PeripheralOS is DeviceInfo.OS for peripherals reached over Bluetooth Low
// Energy. Firmware releases for them are uploaded with this OS.
const PeripheralOS = "ble"

// Device statuses.
const (
	DeviceOnline  = "online"
	DeviceOffline = "offline"
)

// EnrollmentToken lets new devices join the fleet.
type EnrollmentToken struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Channel     string            `json:"channel"`
	Labels      map[string]string `json:"labels"`
	MaxUses     int               `json:"max_uses"` // 0 = unlimited
	Uses        int               `json:"uses"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	Revoked     bool              `json:"revoked"`
	CreatedAt   time.Time         `json:"created_at"`
	// Token is only populated in the response to a create call.
	Token string `json:"token,omitempty"`
}

// CreateTokenRequest is the admin request to mint an enrollment token.
type CreateTokenRequest struct {
	Description string            `json:"description"`
	Channel     string            `json:"channel"`
	Labels      map[string]string `json:"labels"`
	MaxUses     int               `json:"max_uses"`
	TTLSeconds  int64             `json:"ttl_seconds"` // 0 = never expires
}

// Release is a versioned, signed artifact for a package.
type Release struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	OS        string `json:"os,omitempty"`   // empty = any
	Arch      string `json:"arch,omitempty"` // empty = any
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Signature string `json:"signature"` // base64 ed25519 over SigningPayload
	Notes     string `json:"notes,omitempty"`
	// VersionCode is the Android versionCode for APK releases (0 otherwise).
	VersionCode int64     `json:"version_code,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// SigningPayload is the canonical message the server signs for a release.
// Binding package, version and digest together means a valid artifact cannot
// be replayed under a different package or version.
func SigningPayload(pkg, version, sha256hex string) []byte {
	return []byte("armada-release-v1\n" + pkg + "\n" + version + "\n" + sha256hex + "\n")
}

// Selector picks the devices a deployment targets. All non-empty fields must match.
type Selector struct {
	Channel   string            `json:"channel,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	DeviceIDs []string          `json:"device_ids,omitempty"`
}

// Strategy controls how fast a deployment rolls out and when it stops itself.
type Strategy struct {
	// Stages are cumulative percentages of targeted devices, e.g. [5, 25, 100].
	Stages []int `json:"stages"`
	// StageDurationSeconds is the soak time before auto-advancing to the next stage.
	StageDurationSeconds int64 `json:"stage_duration_seconds"`
	// AutoAdvance moves to the next stage automatically once the soak time passes.
	AutoAdvance bool `json:"auto_advance"`
	// MaxFailureRate (0..1) pauses the deployment once exceeded.
	MaxFailureRate float64 `json:"max_failure_rate"`
	// MinSamples is how many finished updates are needed before the failure rate is evaluated.
	MinSamples int `json:"min_samples"`
	// AllowDowngrade permits installing a version lower than the one on the device (rollbacks).
	AllowDowngrade bool `json:"allow_downgrade"`

	// StartAt schedules the rollout: it stays "scheduled" until then.
	StartAt *time.Time `json:"start_at,omitempty"`
	// Windows limits when the rollout offers updates to new devices and
	// advances stages, e.g. "22:00-05:00" or "01:00-04:00,13:00-14:00".
	// Installs already under way finish regardless. Empty means any time.
	Windows string `json:"windows,omitempty"`
	// Days limits windows to weekdays ("mon".."sun"). An overnight window
	// belongs to the day it starts on.
	Days []string `json:"days,omitempty"`
	// TimeZone (IANA name) the windows are in; default UTC. Devices also
	// honour their own maintenance_window.
	TimeZone string `json:"time_zone,omitempty"`
}

// Deployment rolls a release out to a set of devices.
type Deployment struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	ReleaseID      string    `json:"release_id"`
	Package        string    `json:"package"`
	Version        string    `json:"version"`
	Selector       Selector  `json:"selector"`
	Strategy       Strategy  `json:"strategy"`
	Status         string    `json:"status"`
	StatusReason   string    `json:"status_reason,omitempty"`
	StageIndex     int       `json:"stage_index"`
	StageStartedAt time.Time `json:"stage_started_at"`
	// AcknowledgedFailures is the failure count at the last admin resume; only
	// failures beyond it count towards MaxFailureRate.
	AcknowledgedFailures int             `json:"acknowledged_failures"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
	Stats                DeploymentStats `json:"stats"`
	// CreatedBy and ApprovedBy name who started and who approved the
	// rollout. With rollout approval on, it can't go past its first
	// (canary) stage until someone else approves it.
	CreatedBy  string     `json:"created_by,omitempty"`
	ApprovedBy string     `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	// NeedsApproval is set in responses while approval is outstanding.
	NeedsApproval bool `json:"needs_approval,omitempty"`
}

// CurrentPercent is the share of targeted devices currently eligible.
func (d *Deployment) CurrentPercent() int {
	if len(d.Strategy.Stages) == 0 {
		return 100
	}
	i := d.StageIndex
	if i >= len(d.Strategy.Stages) {
		i = len(d.Strategy.Stages) - 1
	}
	return d.Strategy.Stages[i]
}

// Deployment statuses.
const (
	DeploymentScheduled = "scheduled"
	// DeploymentAwaitingApproval offers nothing until someone other than
	// its creator approves it (when rollout approval is on).
	DeploymentAwaitingApproval = "awaiting_approval"
	DeploymentRunning          = "running"
	DeploymentPaused           = "paused"
	DeploymentAborted          = "aborted"
	// DeploymentCompleted means an admin retired the rollout: it no longer
	// offers its release, including to devices enrolled later.
	DeploymentCompleted = "completed"
)

// DeploymentStats summarises per-device progress of a deployment.
type DeploymentStats struct {
	Pending     int     `json:"pending"`
	InProgress  int     `json:"in_progress"`
	Succeeded   int     `json:"succeeded"`
	Failed      int     `json:"failed"`
	RolledBack  int     `json:"rolled_back"`
	Total       int     `json:"total"`
	FailureRate float64 `json:"failure_rate"`
}

// CreateDeploymentRequest is the admin request to start a rollout.
type CreateDeploymentRequest struct {
	Name      string    `json:"name"`
	ReleaseID string    `json:"release_id"`
	Selector  Selector  `json:"selector"`
	Strategy  *Strategy `json:"strategy,omitempty"` // nil = server default
}

// DeviceUpdate tracks one device's progress within one deployment.
type DeviceUpdate struct {
	DeploymentID string    `json:"deployment_id"`
	DeviceID     string    `json:"device_id"`
	ReleaseID    string    `json:"release_id"`
	Package      string    `json:"package"`
	Version      string    `json:"version"`
	FromVersion  string    `json:"from_version,omitempty"`
	Status       string    `json:"status"`
	Message      string    `json:"message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Device update statuses.
const (
	UpdatePending     = "pending" // offered to the device, not yet acknowledged
	UpdateDownloading = "downloading"
	UpdateInstalling  = "installing"
	UpdateSucceeded   = "succeeded"
	UpdateFailed      = "failed"
	UpdateRolledBack  = "rolled_back"
)

// IsTerminalUpdateStatus reports whether no further progress is expected.
func IsTerminalUpdateStatus(s string) bool {
	return s == UpdateSucceeded || s == UpdateFailed || s == UpdateRolledBack
}

// Action is a one-off command queued for a device.
type Action struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	Type      string    `json:"type"`
	Args      []string  `json:"args,omitempty"`
	Status    string    `json:"status"`
	ExitCode  int       `json:"exit_code"`
	Output    string    `json:"output,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// BatchID is set when the action was queued by a bulk operation.
	BatchID string `json:"batch_id,omitempty"`
}

// Bulk operations.
const (
	BulkWake         = "wake"
	BulkSetLabels    = "set_labels"
	BulkRemoveLabels = "remove_labels"
	BulkSetChannel   = "set_channel"
	BulkLifecycle    = "lifecycle"
	// Actions: reboot, restart-agent, exec and lock (the action types).
)

// BulkRequest applies one operation to many devices. Run it with dry_run
// first: the answer says how many devices match (and which would be
// skipped, and why). Then run it with expect set to that number; if the
// devices matching have changed meanwhile, nothing happens (409).
type BulkRequest struct {
	Operation string `json:"operation"`
	// Selector picks the devices; empty needs all=true (every device you can see).
	Selector Selector `json:"selector"`
	All      bool     `json:"all,omitempty"`

	Args      []string          `json:"args,omitempty"`       // exec: the command
	Labels    map[string]string `json:"labels,omitempty"`     // set_labels
	LabelKeys []string          `json:"label_keys,omitempty"` // remove_labels
	Channel   string            `json:"channel,omitempty"`    // set_channel
	State     string            `json:"state,omitempty"`      // lifecycle
	Note      string            `json:"note,omitempty"`       // lifecycle (and the record)

	DryRun bool `json:"dry_run,omitempty"`
	Expect *int `json:"expect,omitempty"`
}

// BulkTarget is one device in a bulk operation.
type BulkTarget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason,omitempty"` // why it was skipped
	// ActionID and Status follow an action queued on the device.
	ActionID string `json:"action_id,omitempty"`
	Status   string `json:"status,omitempty"`
}

// BulkOperation records a bulk operation (or previews one).
type BulkOperation struct {
	ID        string       `json:"id,omitempty"` // empty for a dry run
	Operation string       `json:"operation"`
	Summary   string       `json:"summary"`
	DryRun    bool         `json:"dry_run,omitempty"`
	Matched   int          `json:"matched"` // devices matching the selector that you can see
	Applied   []BulkTarget `json:"applied"`
	Skipped   []BulkTarget `json:"skipped"`
	CreatedBy string       `json:"created_by,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	// Progress counts the queued actions by status (on read; actions only).
	Progress map[string]int `json:"progress,omitempty"`
}

// Action types.
const (
	ActionReboot       = "reboot"
	ActionRestartAgent = "restart-agent"
	ActionExec         = "exec"
	// ActionLogs collects recent logs from the device. Args are a source
	// ("journal" or "file") followed by key=value options: unit=, name=,
	// lines=, since= (a duration such as 1h) and grep= (case-insensitive).
	ActionLogs = "logs"
	// ActionLock locks an Android device's screen now.
	ActionLock = "lock"
	// ActionWipe factory-resets an Android device. Args must be
	// ["confirm=<device ID>"]. The device leaves the fleet until it is
	// provisioned again.
	ActionWipe = "wipe"
)

// WaitResponse answers an agent's long-poll.
type WaitResponse struct {
	Wake bool `json:"wake"`
}

// WakeResponse reports whether a device had a live connection when woken.
// If not, it checks in as soon as it reconnects or at its next heartbeat.
type WakeResponse struct {
	Connected bool `json:"connected"`
}

// Action statuses.
const (
	ActionQueued    = "queued"
	ActionSent      = "sent"
	ActionSucceeded = "succeeded"
	ActionFailed    = "failed"
	ActionRejected  = "rejected"
)

// CreateActionRequest queues an action for a device.
type CreateActionRequest struct {
	Type string   `json:"type"`
	Args []string `json:"args,omitempty"`
}

// ActionResult is reported by the agent after running an action.
type ActionResult struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// Event is an entry in the fleet activity log.
type Event struct {
	ID           int64     `json:"id"`
	Time         time.Time `json:"time"`
	Type         string    `json:"type"`
	DeviceID     string    `json:"device_id,omitempty"`
	DeploymentID string    `json:"deployment_id,omitempty"`
	Message      string    `json:"message"`
	// Actor is who caused the event: a user's email, "admin token", or empty for the system.
	Actor string `json:"actor,omitempty"`
}

// ---- Agent protocol ----

// EnrollRequest is sent by an agent the first time it starts.
type EnrollRequest struct {
	Token string     `json:"token"`
	Name  string     `json:"name,omitempty"`
	Info  DeviceInfo `json:"info"`
	// Attestation proves a hardware-backed key. Required for Android devices
	// unless the server is configured otherwise.
	Attestation *AttestationEvidence `json:"attestation,omitempty"`
}

// ChallengeResponse is a single-use nonce for attestation and license requests.
type ChallengeResponse struct {
	Challenge string `json:"challenge"` // base64
	ExpiresIn int    `json:"expires_in"`
}

// AttestationEvidence is an Android Keystore attestation certificate chain.
type AttestationEvidence struct {
	// Challenge is the base64 challenge from POST /api/v1/agent/challenge that
	// was passed to setAttestationChallenge.
	Challenge string `json:"challenge"`
	// Chain is the base64 DER certificate chain, leaf first.
	Chain []string `json:"chain"`
}

// App is an Android app Armada manages: it installs it, and may license it.
type App struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	// Signers are SHA-256 digests (hex) of the app's signing certificates.
	// Devices refuse to install an APK signed by anyone else, and licenses are
	// only issued to callers signed with one of these.
	Signers []string `json:"signers"`
	// Licensing enables device licenses for this app.
	Licensing bool `json:"licensing"`
	// LicenseTTLSeconds is how long a license lasts; revocation takes effect
	// within this time.
	LicenseTTLSeconds int64     `json:"license_ttl_seconds"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// LicenseRequest is sent by the Android agent on behalf of an app on the device.
type LicenseRequest struct {
	AppPackage string `json:"app_package"`
	// AppSigner is the SHA-256 (hex) of the calling app's signing certificate,
	// as read by the agent from the package manager.
	AppSigner string `json:"app_signer"`
	// AppKeyJWK optionally binds the license to the app's own EC P-256 key
	// (DPoP). Its RFC 7638 thumbprint goes in the license's cnf.jkt claim.
	AppKeyJWK json.RawMessage `json:"app_key_jwk,omitempty"`
	Challenge string          `json:"challenge"`
	// Signature is the hardware key's signature (SHA256withECDSA or
	// SHA256withRSA) over LicenseSigningInput.
	Signature string `json:"signature"`
}

// LicenseSigningInput is the exact byte string the device's hardware key signs
// in a LicenseRequest.
func LicenseSigningInput(deviceID, challenge, appPackage, appSigner, jkt string) []byte {
	return []byte("armada-license-request-v1\n" + deviceID + "\n" + challenge + "\n" + appPackage + "\n" + appSigner + "\n" + jkt + "\n")
}

// LicenseResponse carries a signed license (a JWT).
type LicenseResponse struct {
	License   string    `json:"license"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DevicePolicy is applied by the Android agent on every check-in.
type DevicePolicy struct {
	// DisableDebugging turns off USB debugging (adb), the usual way to pull
	// APKs and data off a device.
	DisableDebugging bool `json:"disable_debugging"`
	// BlockUninstall stops users uninstalling managed apps.
	BlockUninstall bool `json:"block_uninstall"`
}

// EnrollResponse hands the agent its identity and the release-signing key.
type EnrollResponse struct {
	DeviceID                 string `json:"device_id"`
	DeviceKey                string `json:"device_key"`
	SigningPublicKey         string `json:"signing_public_key"` // base64 ed25519
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

// HeartbeatRequest is the periodic check-in from an agent.
type HeartbeatRequest struct {
	Info     DeviceInfo        `json:"info"`
	Metrics  Metrics           `json:"metrics"`
	Packages map[string]string `json:"packages"`
	// Busy is true while the agent is applying an update; the server will not offer another.
	Busy bool `json:"busy"`
	// Status is a short human-readable note, e.g. "update to myapp 1.2.0 deferred: outside maintenance window".
	Status string `json:"status,omitempty"`
	// Health holds the latest result of every health check. Nil means "not
	// reported" (older agents); an empty list means "no checks".
	Health []CheckResult `json:"health"`
	// Peripherals are devices this agent can reach (e.g. BLE sensors), reported as a gateway.
	Peripherals []PeripheralReport `json:"peripherals,omitempty"`
	// Config reports every configuration key the agent has applied or
	// failed to apply. Nil means the agent doesn't support configuration.
	Config []ConfigState `json:"config"`
}

// HeartbeatResponse tells the agent what to do next.
type HeartbeatResponse struct {
	HeartbeatIntervalSeconds int                `json:"heartbeat_interval_seconds"`
	Update                   *UpdateInstruction `json:"update,omitempty"`
	Actions                  []Action           `json:"actions,omitempty"`
	// ManagedPackages lists registered Android apps whose versions the agent should report.
	ManagedPackages []string `json:"managed_packages,omitempty"`
	// Policy is sent to Android devices.
	Policy *DevicePolicy `json:"policy,omitempty"`
	// Monitors are the server-defined checks this device should run.
	Monitors []MonitorCheck `json:"monitors,omitempty"`
	// PeripheralUpdates are firmware updates for peripherals behind this gateway.
	PeripheralUpdates []UpdateInstruction `json:"peripheral_updates,omitempty"`
	// Config is set when the device's configuration needs changing.
	Config *ConfigSync `json:"config,omitempty"`
}

// PeripheralReport describes a device a gateway can reach.
type PeripheralReport struct {
	// Address identifies the peripheral, e.g. a BLE MAC address "C0:98:E5:49:00:01".
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	// Model is the hardware model; firmware releases use it as their package name.
	Model string `json:"model"`
	// Protocol is the firmware update method, e.g. mcumgr, nordic-dfu, esp-ota, or a custom driver name.
	Protocol        string            `json:"protocol,omitempty"`
	FirmwareVersion string            `json:"firmware_version,omitempty"`
	RSSI            int               `json:"rssi,omitempty"`
	BatteryPercent  int               `json:"battery_percent,omitempty"` // -1 or 0 if unknown
	Details         map[string]string `json:"details,omitempty"`
}

// UpdateInstruction asks the agent to install a release.
type UpdateInstruction struct {
	DeploymentID string `json:"deployment_id"`
	ReleaseID    string `json:"release_id"`
	Package      string `json:"package"`
	Version      string `json:"version"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	Signature    string `json:"signature"`
	ArtifactPath string `json:"artifact_path"` // relative to the server URL
	// VersionCode and Signers let an Android agent check the APK itself
	// (package name, versionCode, signing certificate) before installing it.
	VersionCode int64    `json:"version_code,omitempty"`
	Signers     []string `json:"signers,omitempty"`
	// Peripheral is set when the update is for a peripheral behind the gateway.
	Peripheral *PeripheralTarget `json:"peripheral,omitempty"`
}

// ChunkIndex lists a release artifact's content-defined chunks, in order.
// Agents rebuild a new artifact from chunks of the version they have and
// download only the missing ones (delta updates).
type ChunkIndex struct {
	Algorithm string     `json:"algorithm"`
	Size      int64      `json:"size"`
	Chunks    []ChunkRef `json:"chunks"`
}

// ChunkRef is one chunk: its length and truncated SHA-256.
type ChunkRef struct {
	Len int64  `json:"l"`
	Sum string `json:"h"`
}

// PeripheralTarget says which peripheral a gateway should flash.
type PeripheralTarget struct {
	DeviceID        string `json:"device_id"`
	Address         string `json:"address"`
	Model           string `json:"model"`
	Protocol        string `json:"protocol,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
}

// UpdateReport is sent by the agent as an update progresses.
type UpdateReport struct {
	// DeviceID is set by gateways reporting on behalf of a peripheral.
	DeviceID     string `json:"device_id,omitempty"`
	DeploymentID string `json:"deployment_id"`
	ReleaseID    string `json:"release_id"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
}

// ErrorResponse is the JSON body of every non-2xx API response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// FleetSummary powers the dashboard overview.
type FleetSummary struct {
	Devices           int `json:"devices"`
	Online            int `json:"online"`
	Offline           int `json:"offline"`
	ActiveDeployments int `json:"active_deployments"`
	Releases          int `json:"releases"`
	Peripherals       int `json:"peripherals"`
	HealthWarning     int `json:"health_warning"`
	HealthCritical    int `json:"health_critical"`
	FiringAlerts      int `json:"firing_alerts"`
	// Spare and InRepair count devices out of service; Online and Offline
	// count active devices only. Retired devices aren't counted.
	Spare           int                       `json:"spare"`
	InRepair        int                       `json:"in_repair"`
	Retired         int                       `json:"retired"`
	PackageVersions map[string]map[string]int `json:"package_versions"`
	Channels        map[string]int            `json:"channels"`
}

// ---- alerting ----

// NotificationChannel is where alerts are sent.
type NotificationChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Type is webhook | slack | email.
	Type      string         `json:"type"`
	Webhook   *WebhookConfig `json:"webhook,omitempty"`
	Slack     *SlackConfig   `json:"slack,omitempty"`
	Email     *EmailConfig   `json:"email,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	// ClientScope makes this a client's channel (e.g. customer=acme): only
	// that client's rules can use it, and its users can manage it. Client
	// webhooks must use public HTTPS addresses; client email goes through
	// the server's SMTP relay. Empty means a vendor (fleet-wide) channel.
	ClientScope Selector `json:"client_scope"`
}

// WebhookConfig posts alerts to any HTTP endpoint (PagerDuty, Opsgenie,
// Teams, Discord, your own service…).
type WebhookConfig struct {
	URL     string            `json:"url"`
	Method  string            `json:"method,omitempty"` // default POST
	Headers map[string]string `json:"headers,omitempty"`
	// Secret, if set, signs the body: X-Armada-Signature: sha256=<hex HMAC>.
	Secret string `json:"secret,omitempty"`
	// BodyTemplate is a Go text/template rendering the request body from the
	// notification (see AlertNotification). Empty sends the JSON notification.
	BodyTemplate string `json:"body_template,omitempty"`
}

// SlackConfig posts to a Slack incoming webhook.
type SlackConfig struct {
	WebhookURL string `json:"webhook_url"`
}

// EmailConfig sends alerts over SMTP.
type EmailConfig struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"` // 587 (STARTTLS) by default; 465 uses implicit TLS
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from"`
	To       []string `json:"to"`
}

// Alert rule conditions.
const (
	ConditionDeviceOffline    = "device_offline"
	ConditionHealth           = "health"
	ConditionUpdateFailed     = "update_failed"
	ConditionDeploymentPaused = "deployment_paused"
)

// AlertRule decides when to notify whom.
type AlertRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Condition is device_offline | health | update_failed | deployment_paused.
	Condition string `json:"condition"`
	// Selector limits the rule to some devices (ignored for deployment_paused).
	Selector Selector `json:"selector"`
	// OfflineMinutes is how long a device must be silent (device_offline, default 10).
	OfflineMinutes int `json:"offline_minutes,omitempty"`
	// MinSeverity is warning or critical (health, default critical).
	MinSeverity string `json:"min_severity,omitempty"`
	// CheckKinds limits a health rule to some kinds of check, e.g. ["storage", "power"].
	CheckKinds []string `json:"check_kinds,omitempty"`
	// Channels are notification channel IDs.
	Channels []string `json:"channels"`
	// RepeatMinutes re-sends a still-firing alert (0 = never).
	RepeatMinutes int `json:"repeat_minutes,omitempty"`
	// NotifyResolved also notifies when the condition clears.
	NotifyResolved bool      `json:"notify_resolved"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// ClientScope makes this a client's rule: it only ever matches devices
	// in that scope, notifies that client's channels, and is managed by its
	// users. Client rules support device_offline and health. Empty means a
	// vendor (fleet-wide) rule.
	ClientScope Selector `json:"client_scope"`
	CreatedBy   string   `json:"created_by,omitempty"`
}

// Alert statuses.
const (
	AlertFiring   = "firing"
	AlertResolved = "resolved"
)

// Alert is one occurrence of a rule's condition.
type Alert struct {
	ID             string     `json:"id"`
	RuleID         string     `json:"rule_id"`
	RuleName       string     `json:"rule_name"`
	Condition      string     `json:"condition"`
	Key            string     `json:"key"`
	Status         string     `json:"status"`
	Severity       string     `json:"severity"` // warning | critical
	Summary        string     `json:"summary"`
	DeviceID       string     `json:"device_id,omitempty"`
	DeviceName     string     `json:"device_name,omitempty"`
	DeploymentID   string     `json:"deployment_id,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	LastNotifiedAt time.Time  `json:"last_notified_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	Count          int        `json:"count"` // occurrences folded into this alert
	UpdatedAt      time.Time  `json:"updated_at"`
	// Acknowledged: someone is handling it. Repeat notifications stop until
	// it resolves or gets worse (a severity escalation clears the ack).
	AckedBy string     `json:"acked_by,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
	AckNote string     `json:"ack_note,omitempty"`
	// ClosedOutOfService means it closed because its device left service
	// (spare, repair, retired or removed), not because the problem was fixed.
	ClosedOutOfService bool `json:"closed_out_of_service,omitempty"`
	// InMaintenance is set (on read) when the alert's device is in a
	// maintenance window: the alert neither repeats nor resolves meanwhile.
	InMaintenance bool `json:"in_maintenance,omitempty"`
}

// AckRequest acknowledges a firing alert.
type AckRequest struct {
	Note string `json:"note,omitempty"`
}

// Maintenance is a planned period during which devices are expected to be
// down or noisy: alerts for them are held and updates are not offered.
type Maintenance struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
	// Selector picks the devices (device IDs, labels, channel); it must not be empty.
	Selector Selector `json:"selector"`
	// StartsAt/EndsAt bound the maintenance. Both are required for a
	// one-off window; a recurring one may leave EndsAt empty (no end).
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	// Windows, Days and TimeZone make it recurring, e.g. "02:00-04:00" on
	// ["sun"] in "Europe/Berlin": it is active only inside those windows
	// (and between StartsAt and EndsAt).
	Windows  string   `json:"windows,omitempty"`
	Days     []string `json:"days,omitempty"`
	TimeZone string   `json:"time_zone,omitempty"`
	// SuppressAlerts holds alerts for covered devices (default true).
	SuppressAlerts *bool `json:"suppress_alerts,omitempty"`
	// PauseUpdates stops rollouts offering updates to covered devices (default true).
	PauseUpdates *bool `json:"pause_updates,omitempty"`
	// ClientScope is set when a client account created it (or it was made
	// for a client): it only ever covers that client's devices.
	ClientScope Selector  `json:"client_scope"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// Active and State are computed on read: state is scheduled | active |
	// waiting (recurring, between windows) | ended.
	Active bool   `json:"active"`
	State  string `json:"state,omitempty"`
}

// Holds reports whether the maintenance holds alerts.
func (m *Maintenance) Holds() bool { return m.SuppressAlerts == nil || *m.SuppressAlerts }

// Pauses reports whether the maintenance pauses update offers.
func (m *Maintenance) Pauses() bool { return m.PauseUpdates == nil || *m.PauseUpdates }

// Recurring reports whether the maintenance repeats in windows.
func (m *Maintenance) Recurring() bool { return m.Windows != "" || len(m.Days) > 0 }

// MaintenanceInfo annotates a device covered by an active maintenance.
type MaintenanceInfo struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Until  *time.Time `json:"until,omitempty"` // end of the current window, if known
	Note   string     `json:"note,omitempty"`
	Alerts bool       `json:"alerts_held"`
	Update bool       `json:"updates_paused"`
}

// AlertNotification is what channels receive (and what webhook templates render).
type AlertNotification struct {
	Version      string            `json:"version"` // "1"
	AlertID      string            `json:"alert_id"`
	Status       string            `json:"status"` // firing | resolved | test
	Severity     string            `json:"severity"`
	Rule         string            `json:"rule"`
	Condition    string            `json:"condition"`
	Summary      string            `json:"summary"`
	DeviceID     string            `json:"device_id,omitempty"`
	DeviceName   string            `json:"device_name,omitempty"`
	DeviceLabels map[string]string `json:"device_labels,omitempty"`
	DeploymentID string            `json:"deployment_id,omitempty"`
	URL          string            `json:"url,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	ResolvedAt   *time.Time        `json:"resolved_at,omitempty"`
	Count        int               `json:"count"`
}

// ---- history ----

// HistoryPoint is one sample (raw) or one hourly roll-up of a series.
type HistoryPoint struct {
	T   int64    `json:"t"` // unix seconds (start of the hour for roll-ups)
	V   *float64 `json:"v"` // value, or hourly average
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	S   string   `json:"s"` // status (worst status in a roll-up)
}

// HistorySeries is the history of one check or metric on one device.
type HistorySeries struct {
	DeviceID string `json:"device_id,omitempty"`
	// DeviceName is filled in by fleet-wide queries.
	DeviceName string `json:"device_name,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Unit       string `json:"unit,omitempty"`
	// Numeric is true when the series has values worth charting.
	Numeric bool `json:"numeric"`
	// Availability is the share of the range the check was ok (0..1); for the
	// "online" series, the share of the range the device was checking in.
	Availability *float64       `json:"availability,omitempty"`
	Points       []HistoryPoint `json:"points"`
}

// DeviceHistory is the response of GET /api/v1/devices/{id}/history.
type DeviceHistory struct {
	DeviceID   string          `json:"device_id"`
	From       time.Time       `json:"from"`
	To         time.Time       `json:"to"`
	Resolution int64           `json:"resolution_seconds"`
	Series     []HistorySeries `json:"series"`
}

// FleetPoint is the fleet's state at one time bucket (averages over the bucket).
type FleetPoint struct {
	T        int64   `json:"t"`
	Online   float64 `json:"online"`
	Offline  float64 `json:"offline"`
	Warning  float64 `json:"warning"`
	Critical float64 `json:"critical"`
	Alerts   float64 `json:"alerts"`
}

// FleetHistory is the response of GET /api/v1/history/fleet.
type FleetHistory struct {
	From       time.Time    `json:"from"`
	To         time.Time    `json:"to"`
	Resolution int64        `json:"resolution_seconds"`
	Points     []FleetPoint `json:"points"`
}

// ---- users ----

// Roles, from least to most privileged.
const (
	RoleViewer   = "viewer"   // read-only
	RoleOperator = "operator" // device actions, rollouts, releases, monitors, enrollment
	RoleAdmin    = "admin"    // everything, including users, apps, alerting and backups
)

// RoleRank orders roles.
func RoleRank(r string) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// User is a person who signs in to the dashboard or uses the CLI.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	// Scope limits the user to matching devices (e.g. labels customer=acme).
	// Empty means the whole fleet. Scoped users never see fleet-wide pages.
	Scope       Selector   `json:"scope"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	// TwoFactor is true once the user has set up an authenticator app.
	TwoFactor bool `json:"two_factor"`
	// SSO is true once the user has signed in with single sign-on.
	SSO bool `json:"sso"`
	// NoPassword marks users who can only sign in with single sign-on.
	NoPassword bool `json:"no_password,omitempty"`
	// Passkeys is how many passkeys (or security keys) the user has.
	Passkeys int `json:"passkeys"`
	// Provisioned users are managed by your directory over SCIM: it sets
	// their name, active state and (with group mapping) their role.
	Provisioned bool `json:"provisioned,omitempty"`
	// Groups are the user's directory groups (from SCIM, or the SSO groups
	// claim at sign-in).
	Groups        []string `json:"groups,omitempty"`
	GroupsFromSSO bool     `json:"groups_from_sso,omitempty"`
	// GroupManaged means role and device access come from group mappings.
	GroupManaged bool `json:"group_managed,omitempty"`
	// Grants give per-client access: a role on the devices matching each
	// grant's scope (an empty scope is the whole fleet). Set by group
	// mappings for group-managed users, or by an admin. When empty, Role
	// and Scope are the user's single grant.
	Grants []Grant `json:"grants,omitempty"`
	// NoGroupAccess means the user is in no mapped group and can't sign in.
	NoGroupAccess bool       `json:"no_group_access,omitempty"`
	ExternalID    string     `json:"external_id,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// Grant is a role on some devices: those matching Scope, or the whole
// fleet when Scope is empty. On a device, the highest matching grant wins.
type Grant struct {
	Role  string   `json:"role"`
	Scope Selector `json:"scope"`
}

// GroupMapping gives members of a directory group a role and, optionally,
// limits them to some devices (client accounts).
type GroupMapping struct {
	ID    string `json:"id"`
	Group string `json:"group"` // matched case-insensitively
	Role  string `json:"role"`
	// Scope limits members to matching devices; empty means the whole fleet.
	Scope     Selector  `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
	// FromFlag marks mappings set with -oidc-group-roles (read-only here).
	FromFlag bool `json:"from_flag,omitempty"`
}

// SCIMToken authenticates an identity provider's SCIM client.
type SCIMToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Token      string     `json:"token,omitempty"` // only in the create response
	// URL is the SCIM base URL to give the identity provider (create response).
	URL string `json:"url,omitempty"`
}

// SCIMGroup is a directory group pushed over SCIM. Groups named in the
// group-to-role mapping set their members' roles.
type SCIMGroup struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	ExternalID  string    `json:"external_id,omitempty"`
	Members     []string  `json:"members"` // user IDs
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Passkey is a registered passkey or security key.
type Passkey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// Synced passkeys are backed up by a password manager or platform
	// (iCloud Keychain, Google Password Manager…); others live on one device.
	Synced bool `json:"synced"`
}

// PasskeyCeremony starts a WebAuthn registration or sign-in: pass Options
// to navigator.credentials.create() or .get(), then send the result back
// with Ceremony.
type PasskeyCeremony struct {
	Ceremony string `json:"ceremony"`
	Options  any    `json:"options"`
}

// PasskeyFinish completes a WebAuthn ceremony.
type PasskeyFinish struct {
	Ceremony   string          `json:"ceremony"`
	Challenge  string          `json:"challenge,omitempty"` // second-factor sign-in only
	Name       string          `json:"name,omitempty"`      // registration only
	Credential json.RawMessage `json:"credential"`
}

// CreateUserRequest creates a user (admin only).
type CreateUserRequest struct {
	Email string   `json:"email"`
	Name  string   `json:"name"`
	Role  string   `json:"role"`
	Scope Selector `json:"scope"`
	// Grants, if set, give per-client roles instead of Role and Scope.
	Grants   []Grant `json:"grants,omitempty"`
	Password string  `json:"password"`
}

// LoginRequest signs in with email and password.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// Code is an authenticator or recovery code, for one-step sign-in from
	// scripts. The dashboard sends it in a second step instead.
	Code string `json:"code,omitempty"`
}

// LoginResponse returns a session token for the Authorization header.
type LoginResponse struct {
	Token     string    `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	User      User      `json:"user"`
	// TwoFactorRequired means the password was right and a code is needed:
	// send it with Challenge to POST /api/v1/auth/2fa (or retry the login
	// with "code" set).
	TwoFactorRequired bool   `json:"two_factor_required,omitempty"`
	Challenge         string `json:"challenge,omitempty"`
	// TwoFactorMethods lists what the user can use: "code" and/or "passkey".
	TwoFactorMethods []string `json:"two_factor_methods,omitempty"`
}

// TwoFactorLoginRequest completes a sign-in with an authenticator or recovery code.
type TwoFactorLoginRequest struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

// AuthConfig tells the sign-in page which methods are available.
type AuthConfig struct {
	PasswordLogin bool `json:"password_login"`
	// Passkeys means passwordless passkey sign-in is available.
	Passkeys bool   `json:"passkeys"`
	SSO      bool   `json:"sso"`
	SSOName  string `json:"sso_name,omitempty"` // e.g. "Google" or "Okta"
}

// TwoFactorSetup starts authenticator setup. Confirm with a code to enable.
type TwoFactorSetup struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`    // otpauth:// link
	QRPNG  []byte `json:"qr_png"` // the link as a QR code
}

// RecoveryCodes are shown once; each signs in a single time without the app.
type RecoveryCodes struct {
	Codes []string `json:"codes"`
}

// Me describes the signed-in principal.
type Me struct {
	User *User `json:"user,omitempty"` // nil for the server's admin token
	// Name is who actions are attributed to.
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Scope  Selector `json:"scope"`
	Scoped bool     `json:"scoped"`
	// Scopes lists every device scope the user has (any one matching is
	// enough). Scope is the first, for older clients.
	Scopes []Selector `json:"scopes,omitempty"`
	// Grants is the full picture: a role per set of devices. Role is the
	// highest of them; FleetRole the role on the whole fleet ("" if none).
	Grants    []Grant `json:"grants"`
	FleetRole string  `json:"fleet_role"`
	// SignInMethod is "password", "sso", "token" or "admin-token".
	SignInMethod string `json:"sign_in_method"`
	// TwoFactorSetupRequired means the server requires two-factor sign-in
	// and this user must set it up before doing anything else.
	TwoFactorSetupRequired bool `json:"two_factor_setup_required,omitempty"`
}

// APIToken is a personal token for scripts and the CLI.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Token      string     `json:"token,omitempty"` // only in the create response
}

// ---- reports ----

// Report summarises a set of devices (a client's, or the whole fleet) over a period.
type Report struct {
	Title       string              `json:"title"`
	Brand       string              `json:"brand,omitempty"`
	Scope       Selector            `json:"scope"`
	From        time.Time           `json:"from"`
	To          time.Time           `json:"to"`
	TimeZone    string              `json:"time_zone"`
	GeneratedAt time.Time           `json:"generated_at"`
	Summary     ReportSummary       `json:"summary"`
	Devices     []ReportDevice      `json:"devices"`
	Alerts      []ReportAlert       `json:"alerts"`
	AlertRules  []ReportCount       `json:"alert_rules"`
	Updates     []ReportCount       `json:"updates"`
	Changes     []Event             `json:"changes"`
	Maintenance []ReportMaintWindow `json:"maintenance"`
	// Notes explain what the numbers do and don't include.
	Notes []string `json:"notes"`
}

// ReportSummary is the headline numbers of a report.
type ReportSummary struct {
	Devices  int `json:"devices"`
	Active   int `json:"active"`
	Spare    int `json:"spare"`
	InRepair int `json:"in_repair"`
	Retired  int `json:"retired"` // retired during the period
	// Availability is the average over active devices (percent), if history is kept.
	Availability     *float64 `json:"availability,omitempty"`
	BelowTarget      int      `json:"below_target"` // active devices under AvailabilityTarget
	AvailabilityGoal float64  `json:"availability_target"`
	AlertsCritical   int      `json:"alerts_critical"`
	AlertsWarning    int      `json:"alerts_warning"`
	// MeanTimeToResolveMinutes is over alerts that resolved in the period.
	MeanTimeToResolveMinutes *float64       `json:"mean_time_to_resolve_minutes,omitempty"`
	UpdatesSucceeded         int            `json:"updates_succeeded"`
	UpdatesFailed            int            `json:"updates_failed"`
	Replacements             int            `json:"replacements"`
	MaintenanceWindows       int            `json:"maintenance_windows"`
	HealthNow                map[string]int `json:"health_now"`
}

// ReportDevice is one device's line in a report.
type ReportDevice struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Lifecycle        string            `json:"lifecycle"`
	Platform         string            `json:"platform"`
	Availability     *float64          `json:"availability,omitempty"`
	DowntimeMinutes  *int              `json:"downtime_minutes,omitempty"`
	Alerts           int               `json:"alerts"`
	UpdatesSucceeded int               `json:"updates_succeeded"`
	UpdatesFailed    int               `json:"updates_failed"`
	Health           string            `json:"health"`
	Packages         map[string]string `json:"packages"`
	LastSeenAt       time.Time         `json:"last_seen_at"`
}

// ReportAlert is an alert that started in the period.
type ReportAlert struct {
	Rule            string     `json:"rule"`
	Severity        string     `json:"severity"`
	DeviceID        string     `json:"device_id,omitempty"`
	DeviceName      string     `json:"device_name,omitempty"`
	Summary         string     `json:"summary"`
	StartedAt       time.Time  `json:"started_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	DurationMinutes *int       `json:"duration_minutes,omitempty"`
	// ClosedOutOfService: it closed because the device left service.
	ClosedOutOfService bool `json:"closed_out_of_service,omitempty"`
}

// ReportCount counts something by name (alerts by rule, updates by version).
type ReportCount struct {
	Name   string `json:"name"`
	Count  int    `json:"count"`
	Failed int    `json:"failed,omitempty"`
}

// ReportMaintWindow is a maintenance window that overlapped the period.
type ReportMaintWindow struct {
	Name     string     `json:"name"`
	Note     string     `json:"note,omitempty"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
	Windows  string     `json:"windows,omitempty"`
	Devices  int        `json:"devices"`
}

// ReportSchedule emails a report every week or month.
type ReportSchedule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ClientScope is the client the report is about; empty is the whole fleet (admins only).
	ClientScope Selector  `json:"client_scope"`
	Period      string    `json:"period"` // weekly (Monday to Sunday) | monthly
	TimeZone    string    `json:"time_zone,omitempty"`
	Recipients  []string  `json:"recipients"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// LastPeriod is the start date of the last period sent ("2026-10-01").
	LastPeriod    string     `json:"last_period,omitempty"`
	LastSentAt    *time.Time `json:"last_sent_at,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}
