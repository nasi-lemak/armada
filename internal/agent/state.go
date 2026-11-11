package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// PendingSelfUpdate survives the restart into a new agent binary so the new
// process can confirm the update (or the old one can report that it failed).
type PendingSelfUpdate struct {
	DeploymentID string    `json:"deployment_id"`
	ReleaseID    string    `json:"release_id"`
	Version      string    `json:"version"`
	FromVersion  string    `json:"from_version"`
	BackupPath   string    `json:"backup_path"`
	Starts       int       `json:"starts"`
	StartedAt    time.Time `json:"started_at"`
}

// InstalledPackage records what the agent last installed for a package.
type InstalledPackage struct {
	Version          string `json:"version"`
	Artifact         string `json:"artifact,omitempty"`
	PreviousVersion  string `json:"previous_version,omitempty"`
	PreviousArtifact string `json:"previous_artifact,omitempty"`
}

// State is persisted to <state_dir>/state.json.
type State struct {
	DeviceID         string                      `json:"device_id"`
	DeviceKey        string                      `json:"device_key"`
	SigningPublicKey string                      `json:"signing_public_key"`
	Installed        map[string]InstalledPackage `json:"installed"`
	SelfUpdate       *PendingSelfUpdate          `json:"self_update,omitempty"`
	// Outbox holds update reports that could not be delivered yet.
	Outbox []api.UpdateReport `json:"outbox,omitempty"`
}

type stateFile struct {
	mu   sync.Mutex
	path string
	s    State
}

func loadState(dir string) (*stateFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sf := &stateFile{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(sf.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &sf.s); err != nil {
			return nil, err
		}
	}
	if sf.s.Installed == nil {
		sf.s.Installed = map[string]InstalledPackage{}
	}
	return sf, nil
}

// update mutates state under the lock and persists it atomically.
func (sf *stateFile) update(fn func(*State)) error {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	fn(&sf.s)
	return sf.saveLocked()
}

func (sf *stateFile) get() State {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	s := sf.s
	s.Installed = make(map[string]InstalledPackage, len(sf.s.Installed))
	for k, v := range sf.s.Installed {
		s.Installed[k] = v
	}
	s.Outbox = append([]api.UpdateReport(nil), sf.s.Outbox...)
	if sf.s.SelfUpdate != nil {
		p := *sf.s.SelfUpdate
		s.SelfUpdate = &p
	}
	return s
}

func (sf *stateFile) saveLocked() error {
	b, err := json.MarshalIndent(sf.s, "", "  ")
	if err != nil {
		return err
	}
	tmp := sf.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, sf.path)
}
