package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// configMu serialises configuration changes (they may run reload commands).
var configMu sync.Mutex

// configReport lists every configuration key for the heartbeat. It is never
// nil, which tells the server this agent supports configuration profiles.
func (a *Agent) configReport() []api.ConfigState {
	st := a.state.get()
	out := make([]api.ConfigState, 0, len(st.Config))
	for k, c := range st.Config {
		out = append(out, api.ConfigState{Key: k, Hash: c.Hash, Status: c.Status, Message: c.Message, ProfileID: c.ProfileID, UpdatedAt: c.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// applyConfig brings local configuration in line with the server's.
func (a *Agent) applyConfig(ctx context.Context, sync *api.ConfigSync) {
	configMu.Lock()
	defer configMu.Unlock()
	changed := false
	for key, ac := range a.state.get().Config {
		if _, ok := sync.Desired[key]; !ok {
			a.log.Info("removing configuration", "key", key)
			if err := a.removeConfig(ctx, key, ac); err != nil {
				a.log.Warn("removing configuration failed", "key", key, "err", err)
			}
			_ = a.state.update(func(s *State) { delete(s.Config, key) })
			changed = true
		}
	}
	for _, it := range sync.Items {
		if sync.Desired[it.Key] != it.Hash {
			continue // stale item
		}
		prev, had := a.state.get().Config[it.Key]
		if had && prev.Hash == it.Hash && prev.Status == api.ConfigApplied {
			continue
		}
		next := AppliedConfig{Hash: it.Hash, ProfileID: it.ProfileID, Status: api.ConfigApplied, UpdatedAt: a.now().UTC()}
		var err error
		switch it.Kind {
		case api.ConfigFiles:
			next.Files, err = a.applyFiles(ctx, it, prev.Files)
			if err != nil {
				next.Files = prev.Files // the previous files were restored
			}
		case api.ConfigAgent:
			if !*a.cfg.AllowRemoteSettings {
				err = errors.New("remote settings are disabled on this device (allow_remote_settings=false)")
			} else {
				next.Agent = it.Agent
			}
		default:
			err = fmt.Errorf("configuration kind %q is not supported by this agent", it.Kind)
		}
		if err != nil {
			next.Status, next.Message = api.ConfigFailed, err.Error()
			a.log.Warn("configuration failed", "key", it.Key, "err", err)
		} else {
			a.log.Info("configuration applied", "key", it.Key)
		}
		_ = a.state.update(func(s *State) {
			if s.Config == nil {
				s.Config = map[string]AppliedConfig{}
			}
			s.Config[it.Key] = next
		})
		changed = true
	}
	if changed {
		select { // report the outcome now, not at the next heartbeat
		case a.wake <- struct{}{}:
		default:
		}
	}
}

func (a *Agent) removeConfig(ctx context.Context, key string, ac AppliedConfig) error {
	kind, target, _ := strings.Cut(key, ":")
	if kind != api.ConfigFiles {
		return nil // settings overrides simply stop applying
	}
	mc, ok := a.cfg.ManagedConfig[target]
	if !ok {
		return nil
	}
	for _, name := range ac.Files {
		if err := os.Remove(filepath.Join(mc.Dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return a.reloadConfig(ctx, mc, key)
}

type fileBackup struct {
	existed bool
	content []byte
	mode    fs.FileMode
}

// applyFiles writes a files item, removes files the profile no longer has,
// and runs the reload command. On any failure the previous files are put
// back (and reloaded), so a bad configuration doesn't stick.
func (a *Agent) applyFiles(ctx context.Context, it api.ConfigItem, prevFiles []string) ([]string, error) {
	mc, ok := a.cfg.ManagedConfig[it.Target]
	if !ok {
		names := make([]string, 0, len(a.cfg.ManagedConfig))
		for n := range a.cfg.ManagedConfig {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("no managed_config entry named %q on this device (configured: %s)", it.Target, strings.Join(names, ", "))
	}
	if err := os.MkdirAll(mc.Dir, 0o755); err != nil {
		return nil, err
	}
	backups := map[string]fileBackup{}
	backup := func(name string) error {
		if _, done := backups[name]; done {
			return nil
		}
		p := filepath.Join(mc.Dir, name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			backups[name] = fileBackup{}
			return nil
		} else if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file", p)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		backups[name] = fileBackup{existed: true, content: b, mode: fi.Mode().Perm()}
		return nil
	}
	restore := func() {
		for name, b := range backups {
			p := filepath.Join(mc.Dir, name)
			if b.existed {
				_ = writeFileAtomic(p, b.content, b.mode)
			} else {
				_ = os.Remove(p)
			}
		}
	}
	var names []string
	want := map[string]bool{}
	for _, f := range it.Files {
		if strings.ContainsAny(f.Name, `/\`) || f.Name == "." || f.Name == ".." || f.Name == "" {
			restore()
			return nil, fmt.Errorf("invalid file name %q", f.Name)
		}
		mode, err := strconv.ParseUint(f.Mode, 8, 32)
		if err != nil || f.Mode == "" {
			mode = 0o644
		}
		if err := backup(f.Name); err != nil {
			restore()
			return nil, err
		}
		if err := writeFileAtomic(filepath.Join(mc.Dir, f.Name), []byte(f.Content), fs.FileMode(mode)&0o777); err != nil {
			restore()
			return nil, err
		}
		names = append(names, f.Name)
		want[f.Name] = true
	}
	for _, old := range prevFiles {
		if want[old] {
			continue
		}
		if err := backup(old); err != nil {
			restore()
			return nil, err
		}
		if err := os.Remove(filepath.Join(mc.Dir, old)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			restore()
			return nil, err
		}
	}
	if err := a.reloadConfig(ctx, mc, it.Key); err != nil {
		restore()
		if rerr := a.reloadConfig(ctx, mc, it.Key); rerr != nil {
			return nil, fmt.Errorf("%v; previous files restored, but reloading them also failed: %v", err, rerr)
		}
		return nil, fmt.Errorf("%v; previous files restored", err)
	}
	sort.Strings(names)
	return names, nil
}

func (a *Agent) reloadConfig(ctx context.Context, mc ManagedConfigTarget, key string) error {
	if len(mc.ReloadCommand) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, code, err := runCommand(cctx, mc.ReloadCommand, []string{"ARMADA_CONFIG_DIR=" + mc.Dir, "ARMADA_CONFIG_KEY=" + key})
	if err != nil || code != 0 {
		return fmt.Errorf("reload command failed: %s", describeRun(out, code, err))
	}
	return nil
}

func writeFileAtomic(path string, b []byte, mode fs.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".armada-tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil { // umask may have narrowed it
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// settingsOverride returns the server's agent-settings override, if any.
func (a *Agent) settingsOverride() *api.AgentSettings {
	if !*a.cfg.AllowRemoteSettings {
		return nil
	}
	if ac, ok := a.state.get().Config[api.ConfigAgent]; ok && ac.Status == api.ConfigApplied {
		return ac.Agent
	}
	return nil
}

// autoUpdate is the effective auto_update setting.
func (a *Agent) autoUpdate() bool {
	if o := a.settingsOverride(); o != nil && o.AutoUpdate != nil {
		return *o.AutoUpdate
	}
	return *a.cfg.AutoUpdate
}

// maintenanceWindow is the effective window, as text and parsed.
func (a *Agent) maintenanceWindow() (string, []Window) {
	if o := a.settingsOverride(); o != nil && o.MaintenanceWindow != nil {
		ws, err := ParseWindows(*o.MaintenanceWindow)
		if err == nil {
			return *o.MaintenanceWindow, ws
		}
	}
	return a.cfg.MaintenanceWindow, a.windows
}
