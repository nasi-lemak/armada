// Package api defines the wire types shared by the Armada server, agent and CLI.
package api

import "time"

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
}

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
	ID        string    `json:"id"`
	Package   string    `json:"package"`
	Version   string    `json:"version"`
	Channel   string    `json:"channel"`
	OS        string    `json:"os,omitempty"`   // empty = any
	Arch      string    `json:"arch,omitempty"` // empty = any
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	Signature string    `json:"signature"` // base64 ed25519 over SigningPayload
	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"created_at"`
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
}

// HeartbeatResponse tells the agent what to do next.
type HeartbeatResponse struct {
	HeartbeatIntervalSeconds int                `json:"heartbeat_interval_seconds"`
	Update                   *UpdateInstruction `json:"update,omitempty"`
	Actions                  []Action           `json:"actions,omitempty"`
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
}

// UpdateReport is sent by the agent as an update progresses.
type UpdateReport struct {
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
