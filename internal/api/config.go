package api

import "time"

// Config profile kinds.
const (
	// ConfigFiles writes files into a directory the agent's managed_config
	// names (Target), then runs that entry's reload command.
	ConfigFiles = "files"
	// ConfigAndroidApp sets an Android app's managed configuration
	// (Target is the package name; Values become its restrictions bundle).
	ConfigAndroidApp = "android_app"
	// ConfigAndroidSystemUpdate sets the Android OS update policy.
	ConfigAndroidSystemUpdate = "android_system_update"
	// ConfigAgent overrides agent settings: maintenance window, auto-update.
	ConfigAgent = "agent"
)

// ConfigProfile is configuration the server keeps applied on matching
// devices. When several enabled profiles of the same kind and target match
// a device, the one with the highest priority wins (then the most recently
// updated); profiles are never merged.
type ConfigProfile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Target   string   `json:"target,omitempty"`
	Selector Selector `json:"selector"`
	Priority int      `json:"priority"`
	Enabled  bool     `json:"enabled"`

	Files        []ConfigFile        `json:"files,omitempty"`
	Values       map[string]any      `json:"values,omitempty"`
	SystemUpdate *SystemUpdatePolicy `json:"system_update,omitempty"`
	Agent        *AgentSettings      `json:"agent,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Compliance counts matching devices by state (only in list/get responses).
	Compliance map[string]int `json:"compliance,omitempty"`
}

// ConfigFile is one file of a files profile.
type ConfigFile struct {
	// Name is the file name inside the target directory (no slashes).
	Name    string `json:"name"`
	Content string `json:"content"`
	// Mode is the octal permission, default "0644".
	Mode string `json:"mode,omitempty"`
	// Template renders Content per device with Go text/template:
	// {{.Device.Name}}, {{.Device.ID}}, {{.Device.Channel}},
	// {{index .Device.Labels "site"}}, {{.Device.Info.Hostname}}.
	Template bool `json:"template,omitempty"`
}

// SystemUpdatePolicy controls Android OS updates (DevicePolicyManager).
type SystemUpdatePolicy struct {
	// Type is automatic (install as soon as available), windowed (install
	// within the daily window), or postpone (hold for up to 30 days).
	Type        string `json:"type"`
	WindowStart string `json:"window_start,omitempty"` // "HH:MM", device local time
	WindowEnd   string `json:"window_end,omitempty"`
}

// AgentSettings override the agent's local configuration. Nil fields keep
// the local value.
type AgentSettings struct {
	MaintenanceWindow *string `json:"maintenance_window,omitempty"`
	AutoUpdate        *bool   `json:"auto_update,omitempty"`
}

// ConfigItem is the rendered configuration for one device and one key.
type ConfigItem struct {
	Key          string              `json:"key"` // kind or kind:target
	Kind         string              `json:"kind"`
	Target       string              `json:"target,omitempty"`
	Hash         string              `json:"hash"`
	ProfileID    string              `json:"profile_id"`
	Files        []ConfigFile        `json:"files,omitempty"`
	Values       map[string]any      `json:"values,omitempty"`
	SystemUpdate *SystemUpdatePolicy `json:"system_update,omitempty"`
	Agent        *AgentSettings      `json:"agent,omitempty"`
}

// ConfigSync is sent in a heartbeat response when the device's applied
// configuration differs from what the server wants.
type ConfigSync struct {
	// Desired maps every key the device should have to its hash. Keys the
	// device has applied but which are missing here should be removed.
	Desired map[string]string `json:"desired"`
	// Items carries the full content of keys whose hash the device lacks.
	Items []ConfigItem `json:"items,omitempty"`
}

// Config apply states.
const (
	ConfigApplied = "applied"
	ConfigFailed  = "failed"
	ConfigPending = "pending" // server-side only: not yet reported
)

// ConfigState is a device's report on one configuration key.
type ConfigState struct {
	Key       string    `json:"key"`
	Hash      string    `json:"hash"`
	Status    string    `json:"status"` // applied | failed
	Message   string    `json:"message,omitempty"`
	ProfileID string    `json:"profile_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}
