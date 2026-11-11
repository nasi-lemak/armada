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
}

// Device is a managed machine as seen by the control plane.
type Device struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Channel     string            `json:"channel"`
	Labels      map[string]string `json:"labels"`
	Info        DeviceInfo        `json:"info"`
	Metrics     Metrics           `json:"metrics"`
	Packages    map[string]string `json:"packages"`
	Status      string            `json:"status"` // online | offline
	EnrolledAt  time.Time         `json:"enrolled_at"`
	LastSeenAt  time.Time         `json:"last_seen_at"`
	AgentStatus string            `json:"agent_status,omitempty"` // free-form note from the agent, e.g. "update deferred"
	// Attestation is set for devices that proved a hardware-backed key at enrollment.
	Attestation *DeviceAttestation `json:"attestation,omitempty"`
	// LicenseRevoked stops the device from receiving new app licenses.
	LicenseRevoked bool `json:"license_revoked"`
	// Health is the worst status among Checks: ok | unknown | warning | critical.
	Health string        `json:"health"`
	Checks []CheckResult `json:"checks,omitempty"`
	// ParentID is set for peripherals (e.g. BLE devices) reached through a gateway device.
	ParentID string `json:"parent_id,omitempty"`
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
	DeploymentRunning = "running"
	DeploymentPaused  = "paused"
	DeploymentAborted = "aborted"
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
}

// Action types.
const (
	ActionReboot       = "reboot"
	ActionRestartAgent = "restart-agent"
	ActionExec         = "exec"
)

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
	Devices           int                       `json:"devices"`
	Online            int                       `json:"online"`
	Offline           int                       `json:"offline"`
	ActiveDeployments int                       `json:"active_deployments"`
	Releases          int                       `json:"releases"`
	PackageVersions   map[string]map[string]int `json:"package_versions"`
	Channels          map[string]int            `json:"channels"`
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
