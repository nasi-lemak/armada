package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

var (
	configTargetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	configFileRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	androidPkgRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	hhmmRe         = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

const maxConfigBytes = 1 << 20

// configKey identifies what a profile configures; one profile wins per key.
func configKey(kind, target string) string {
	if target == "" {
		return kind
	}
	return kind + ":" + target
}

func validateConfigProfile(p *api.ConfigProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 128 {
		return errors.New("name is required (up to 128 characters)")
	}
	if err := validLabels(p.Selector.Labels); err != nil {
		return err
	}
	if p.Selector.Channel != "" && !channelRe.MatchString(p.Selector.Channel) {
		return errors.New("invalid channel")
	}
	if p.Priority < -1000 || p.Priority > 1000 {
		return errors.New("priority must be between -1000 and 1000")
	}
	// Only the fields for the profile's kind are kept.
	files, values, su, ag := p.Files, p.Values, p.SystemUpdate, p.Agent
	p.Files, p.Values, p.SystemUpdate, p.Agent = nil, nil, nil, nil
	switch p.Kind {
	case api.ConfigFiles:
		if !configTargetRe.MatchString(p.Target) {
			return errors.New("target must name a managed_config entry in the agent config (letters, digits, . _ -)")
		}
		if len(files) == 0 {
			return errors.New("a files profile needs at least one file")
		}
		size, seen := 0, map[string]bool{}
		for i := range files {
			f := &files[i]
			if !configFileRe.MatchString(f.Name) {
				return fmt.Errorf("invalid file name %q (no slashes; letters, digits, . _ -)", f.Name)
			}
			if seen[f.Name] {
				return fmt.Errorf("file %q listed twice", f.Name)
			}
			seen[f.Name] = true
			if f.Mode == "" {
				f.Mode = "0644"
			}
			if m, err := strconv.ParseUint(f.Mode, 8, 32); err != nil || m > 0o777 {
				return fmt.Errorf("file %q: mode must be octal such as 0644", f.Name)
			}
			if f.Template {
				if _, err := parseConfigTemplate(f.Name, f.Content); err != nil {
					return fmt.Errorf("file %q: %v", f.Name, err)
				}
			}
			size += len(f.Content)
		}
		if size > maxConfigBytes {
			return errors.New("files are larger than 1 MB in total")
		}
		p.Files = files
	case api.ConfigAndroidApp:
		if !androidPkgRe.MatchString(p.Target) {
			return errors.New("target must be an Android package name such as com.example.kiosk")
		}
		if len(values) == 0 {
			return errors.New("an android_app profile needs values")
		}
		for k, v := range values {
			if k == "" || len(k) > 128 {
				return errors.New("invalid value key")
			}
			switch x := v.(type) {
			case string, bool, float64:
			case []any:
				for _, e := range x {
					if _, ok := e.(string); !ok {
						return fmt.Errorf("value %q: lists may only hold strings", k)
					}
				}
			default:
				return fmt.Errorf("value %q must be a string, number, boolean or list of strings", k)
			}
		}
		if len(mustJSONBytes(values)) > maxConfigBytes {
			return errors.New("values are larger than 1 MB")
		}
		p.Values = values
	case api.ConfigAndroidSystemUpdate:
		p.Target = ""
		if su == nil {
			return errors.New("system_update is required")
		}
		switch su.Type {
		case "automatic", "postpone":
			su.WindowStart, su.WindowEnd = "", ""
		case "windowed":
			if !hhmmRe.MatchString(su.WindowStart) || !hhmmRe.MatchString(su.WindowEnd) || su.WindowStart == su.WindowEnd {
				return errors.New("a windowed policy needs window_start and window_end as HH:MM")
			}
		default:
			return errors.New("system_update type must be automatic, windowed or postpone")
		}
		p.SystemUpdate = su
	case api.ConfigAgent:
		p.Target = ""
		if ag == nil || (ag.MaintenanceWindow == nil && ag.AutoUpdate == nil) {
			return errors.New("agent settings need maintenance_window and/or auto_update")
		}
		if ag.MaintenanceWindow != nil {
			if _, err := rollout.ParseWindows(*ag.MaintenanceWindow); err != nil {
				return err
			}
		}
		p.Agent = ag
	default:
		return errors.New("kind must be files, android_app, android_system_update or agent")
	}
	return nil
}

func mustJSONBytes(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func parseConfigTemplate(name, content string) (*template.Template, error) {
	return template.New(name).Option("missingkey=zero").Parse(content)
}

// configApplies reports whether a profile kind makes sense for a device.
func configApplies(kind string, dev *api.Device) bool {
	if dev.ParentID != "" {
		return false
	}
	android := strings.EqualFold(dev.Info.OS, "android")
	switch kind {
	case api.ConfigAndroidApp, api.ConfigAndroidSystemUpdate:
		return android
	case api.ConfigFiles, api.ConfigAgent:
		return !android
	}
	return false
}

// winningProfiles picks, for each key, the profile that applies to a device.
func winningProfiles(profiles []*api.ConfigProfile, dev *api.Device) map[string]*api.ConfigProfile {
	win := map[string]*api.ConfigProfile{}
	for _, p := range profiles {
		if !p.Enabled || !configApplies(p.Kind, dev) || !rollout.Matches(p.Selector, dev) {
			continue
		}
		k := configKey(p.Kind, p.Target)
		cur := win[k]
		if cur == nil || p.Priority > cur.Priority || (p.Priority == cur.Priority && p.UpdatedAt.After(cur.UpdatedAt)) {
			win[k] = p
		}
	}
	return win
}

// renderConfig produces a device's item for a profile.
func renderConfig(p *api.ConfigProfile, dev *api.Device) (api.ConfigItem, error) {
	it := api.ConfigItem{Key: configKey(p.Kind, p.Target), Kind: p.Kind, Target: p.Target, ProfileID: p.ID,
		Values: p.Values, SystemUpdate: p.SystemUpdate, Agent: p.Agent}
	if p.Kind == api.ConfigFiles {
		data := map[string]any{"Device": dev}
		for _, f := range p.Files {
			if f.Template {
				t, err := parseConfigTemplate(f.Name, f.Content)
				if err != nil {
					return it, err
				}
				var buf bytes.Buffer
				if err := t.Execute(&buf, data); err != nil {
					return it, fmt.Errorf("%s: %v", f.Name, err)
				}
				f.Content, f.Template = buf.String(), false
			}
			it.Files = append(it.Files, f)
		}
	}
	// The hash covers the content only, so an identical profile taking over
	// doesn't make devices re-apply anything.
	h := sha256.Sum256(mustJSONBytes(struct {
		K string
		F []api.ConfigFile
		V map[string]any
		S *api.SystemUpdatePolicy
		A *api.AgentSettings
	}{it.Key, it.Files, it.Values, it.SystemUpdate, it.Agent}))
	it.Hash = hex.EncodeToString(h[:16])
	return it, nil
}

// desiredConfig renders everything a device should have.
func (s *Server) desiredConfig(ctx context.Context, dev *api.Device, profiles []*api.ConfigProfile) (map[string]api.ConfigItem, error) {
	if profiles == nil {
		var err error
		if profiles, err = s.store.ListConfigProfiles(ctx); err != nil {
			return nil, err
		}
	}
	out := map[string]api.ConfigItem{}
	for k, p := range winningProfiles(profiles, dev) {
		it, err := renderConfig(p, dev)
		if err != nil {
			s.log.Warn("config profile does not render for device", "profile", p.Name, "device", dev.ID, "err", err)
			continue
		}
		out[k] = it
	}
	return out, nil
}

// configRetryAfter is how long a failed item waits before being resent.
const configRetryAfter = 10 * time.Minute

// syncConfig records what the agent reports and returns what it should change.
func (s *Server) syncConfig(ctx context.Context, dev *api.Device, reported []api.ConfigState, now time.Time) (*api.ConfigSync, error) {
	stored, err := s.store.DeviceConfig(ctx, dev.ID)
	if err != nil {
		return nil, err
	}
	if !sameStates(stored, reported) {
		for i := range reported {
			reported[i].Message = truncate(reported[i].Message, 2000)
			if reported[i].UpdatedAt.IsZero() {
				reported[i].UpdatedAt = now
			}
		}
		if err := s.store.SetDeviceConfig(ctx, dev.ID, reported); err != nil {
			return nil, err
		}
		for _, r := range reported {
			if r.Status == api.ConfigFailed && !hasState(stored, r.Key, r.Hash, api.ConfigFailed) {
				s.event(ctx, "config.failed", dev.ID, "", fmt.Sprintf("%s: configuration %s failed: %s", dev.Name, r.Key, truncate(r.Message, 200)))
			}
		}
	}
	desired, err := s.desiredConfig(ctx, dev, nil)
	if err != nil {
		return nil, err
	}
	have := map[string]api.ConfigState{}
	for _, r := range reported {
		have[r.Key] = r
	}
	sync := &api.ConfigSync{Desired: map[string]string{}}
	changed := false
	for k, it := range desired {
		sync.Desired[k] = it.Hash
		r, ok := have[k]
		switch {
		case !ok || r.Hash != it.Hash:
			sync.Items = append(sync.Items, it)
			changed = true
		case r.Status == api.ConfigFailed && now.Sub(r.UpdatedAt) > configRetryAfter:
			sync.Items = append(sync.Items, it) // try again
			changed = true
		}
	}
	for k := range have {
		if _, ok := desired[k]; !ok {
			changed = true // the agent should remove it
		}
	}
	if !changed {
		return nil, nil
	}
	sort.Slice(sync.Items, func(i, j int) bool { return sync.Items[i].Key < sync.Items[j].Key })
	return sync, nil
}

func sameStates(a, b []api.ConfigState) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]api.ConfigState{}
	for _, x := range a {
		m[x.Key] = x
	}
	for _, y := range b {
		x, ok := m[y.Key]
		if !ok || x.Hash != y.Hash || x.Status != y.Status || x.Message != truncate(y.Message, 2000) {
			return false
		}
	}
	return true
}

func hasState(states []api.ConfigState, key, hash, status string) bool {
	for _, s := range states {
		if s.Key == key && s.Hash == hash && s.Status == status {
			return true
		}
	}
	return false
}

// ---- device view ----

// DeviceConfigView is one configuration key on a device, as the dashboard shows it.
type DeviceConfigView struct {
	Key         string    `json:"key"`
	ProfileID   string    `json:"profile_id,omitempty"`
	ProfileName string    `json:"profile_name,omitempty"`
	Status      string    `json:"status"` // applied | failed | pending | removing
	Message     string    `json:"message,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

func (s *Server) deviceConfigView(ctx context.Context, dev *api.Device, profiles []*api.ConfigProfile) ([]DeviceConfigView, error) {
	states, err := s.store.DeviceConfig(ctx, dev.ID)
	if err != nil {
		return nil, err
	}
	desired, err := s.desiredConfig(ctx, dev, profiles)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
	}
	have := map[string]api.ConfigState{}
	for _, st := range states {
		have[st.Key] = st
	}
	var out []DeviceConfigView
	for k, it := range desired {
		v := DeviceConfigView{Key: k, ProfileID: it.ProfileID, ProfileName: names[it.ProfileID], Status: api.ConfigPending}
		if st, ok := have[k]; ok && st.Hash == it.Hash {
			v.Status, v.Message, v.UpdatedAt = st.Status, st.Message, st.UpdatedAt
		}
		out = append(out, v)
	}
	for k, st := range have {
		if _, ok := desired[k]; !ok {
			out = append(out, DeviceConfigView{Key: k, Status: "removing", UpdatedAt: st.UpdatedAt})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ---- admin API ----

func (s *Server) handleListConfigProfiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ps, err := s.store.ListConfigProfiles(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.addCompliance(ctx, ps); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(ps))
}

// addCompliance counts, per profile, the devices it applies to by state.
func (s *Server) addCompliance(ctx context.Context, ps []*api.ConfigProfile) error {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return err
	}
	all, err := s.store.AllDeviceConfig(ctx)
	if err != nil {
		return err
	}
	byID := map[string]*api.ConfigProfile{}
	for _, p := range ps {
		p.Compliance = map[string]int{api.ConfigApplied: 0, api.ConfigFailed: 0, api.ConfigPending: 0}
		byID[p.ID] = p
	}
	for _, d := range devs {
		reported := map[string]api.ConfigState{}
		for _, st := range all[d.ID] {
			reported[st.Key] = st
		}
		for k, p := range winningProfiles(ps, d) {
			it, err := renderConfig(p, d)
			if err != nil {
				byID[p.ID].Compliance[api.ConfigFailed]++
				continue
			}
			st, ok := reported[k]
			switch {
			case ok && st.Hash == it.Hash && st.Status == api.ConfigApplied:
				byID[p.ID].Compliance[api.ConfigApplied]++
			case ok && st.Hash == it.Hash && st.Status == api.ConfigFailed:
				byID[p.ID].Compliance[api.ConfigFailed]++
			default:
				byID[p.ID].Compliance[api.ConfigPending]++
			}
		}
	}
	return nil
}

func (s *Server) handleGetConfigProfile(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetConfigProfile(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.addCompliance(r.Context(), []*api.ConfigProfile{p}); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handlePutConfigProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var p api.ConfigProfile
	if !readJSONLimit(w, r, &p, 4*maxConfigBytes) {
		return
	}
	now := s.now()
	verb := "created"
	if id := r.PathValue("id"); id != "" {
		old, err := s.store.GetConfigProfile(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "profile not found")
			return
		} else if err != nil {
			s.internalError(w, err)
			return
		}
		p.ID, p.CreatedAt, verb = old.ID, old.CreatedAt, "updated"
	} else {
		p.ID, p.CreatedAt = newID("cfg"), now
	}
	if err := validateConfigProfile(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.UpdatedAt = now
	if err := s.store.UpsertConfigProfile(ctx, &p); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a profile with that name already exists")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.event(ctx, "config."+verb, "", "", fmt.Sprintf("configuration profile %q %s (%s)", p.Name, verb, configKey(p.Kind, p.Target)))
	s.wakeMatching(ctx, p.Selector)
	code := http.StatusOK
	if verb == "created" {
		code = http.StatusCreated
	}
	writeJSON(w, code, p)
}

func (s *Server) handleDeleteConfigProfile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.store.GetConfigProfile(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "profile not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.store.DeleteConfigProfile(ctx, p.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.event(ctx, "config.deleted", "", "", fmt.Sprintf("configuration profile %q deleted; devices remove its configuration", p.Name))
	s.wakeMatching(ctx, p.Selector)
	w.WriteHeader(http.StatusNoContent)
}

// wakeMatching wakes connected devices matching a selector, spread out so a
// large fleet doesn't check in all at once.
func (s *Server) wakeMatching(ctx context.Context, sel api.Selector) {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	var ids []string
	for _, d := range devs {
		if d.ParentID == "" && rollout.Matches(sel, d) {
			ids = append(ids, d.ID)
		}
	}
	s.wakeIDs(ids)
}
