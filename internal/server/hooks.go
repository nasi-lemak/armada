package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// Event webhooks: every activity-log event, as it's recorded, goes to the
// webhooks whose type filter matches, signed with the webhook's secret.
// A client's webhook gets only events about that client's devices (what
// its activity log shows) and may only reach public HTTPS addresses.
// Delivery is in the background with retries; a webhook that keeps
// failing is disabled.

const (
	maxEventHooks     = 50
	hookAttempts      = 4
	hookMaxFailures   = 20 // deliveries in a row (each after retries)
	hookQueueSize     = 2000
	hookEventsSkipped = "hook." // a webhook's own events never go to webhooks (no loops)
)

var hookTypeRe = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*(\.\*|\*)?$|^\*$`)

type hookJob struct {
	hookID  string
	eventID int64
	typ     string
	body    []byte
}

// eventHookState caches the enabled webhooks, so recording an event
// doesn't read them from the database each time.
type eventHookState struct {
	mu     sync.Mutex
	loaded bool
	hooks  []*api.EventHook
	queue  chan hookJob
	done   chan struct{} // closed by Close; the queue stays open so senders never panic
	// retryDelay is how long to wait before attempt n (tests shorten it).
	retryDelay func(attempt int) time.Duration
	statusMu   sync.Mutex // serializes delivery-status writes
}

func (s *Server) startEventHooks() {
	s.hooks.queue = make(chan hookJob, hookQueueSize)
	s.hooks.done = make(chan struct{})
	s.hooks.retryDelay = func(n int) time.Duration { return time.Duration(n*n) * 3 * time.Second }
	go s.hookLoop()
}

func (s *Server) invalidateHooks() {
	s.hooks.mu.Lock()
	s.hooks.loaded = false
	s.hooks.mu.Unlock()
}

func (s *Server) activeHooks(ctx context.Context) []*api.EventHook {
	s.hooks.mu.Lock()
	defer s.hooks.mu.Unlock()
	if !s.hooks.loaded {
		all, err := s.store.ListEventHooks(ctx)
		if err != nil {
			s.log.Error("load event webhooks", "err", err)
			return nil
		}
		s.hooks.hooks = s.hooks.hooks[:0]
		for _, h := range all {
			if h.Enabled {
				s.hooks.hooks = append(s.hooks.hooks, h)
			}
		}
		s.hooks.loaded = true
	}
	return s.hooks.hooks
}

func hookWants(h *api.EventHook, typ string) bool {
	if len(h.Types) == 0 {
		return true
	}
	for _, t := range h.Types {
		if t == "*" || t == typ {
			return true
		}
		if p, ok := strings.CutSuffix(t, "*"); ok && strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// fanoutEvent queues an event for the webhooks that want it.
func (s *Server) fanoutEvent(ctx context.Context, e *api.Event) {
	if s.hooks.queue == nil || strings.HasPrefix(e.Type, hookEventsSkipped) {
		return
	}
	hooks := s.activeHooks(ctx)
	if len(hooks) == 0 {
		return
	}
	var dev *api.Device
	if e.DeviceID != "" {
		if d, err := s.store.GetDevice(ctx, e.DeviceID); err == nil {
			dev = d
		}
	}
	for _, h := range hooks {
		if !hookWants(h, e.Type) {
			continue
		}
		if !scopeEmpty(h.ClientScope) && (dev == nil || !rollout.Matches(h.ClientScope, dev)) {
			continue // clients get events about their own devices only
		}
		body, err := json.Marshal(hookPayload(h, e, dev))
		if err != nil {
			continue
		}
		select {
		case s.hooks.queue <- hookJob{hookID: h.ID, eventID: e.ID, typ: e.Type, body: body}:
		default:
			s.log.Error("event webhook queue full; dropping", "hook", h.Name, "event", e.ID)
		}
	}
}

func hookPayload(h *api.EventHook, e *api.Event, dev *api.Device) api.EventHookPayload {
	p := api.EventHookPayload{Version: "1", ID: e.ID, Time: e.Time.UTC(), Type: e.Type, Message: e.Message, Actor: e.Actor,
		DeploymentID: e.DeploymentID, Hook: h.Name}
	if dev != nil {
		p.Device = &api.HookDevice{ID: dev.ID, Name: dev.Name, Channel: dev.Channel, Labels: dev.Labels}
	}
	if !scopeEmpty(h.ClientScope) {
		p.DeploymentID = "" // rollouts are the vendor's
	}
	return p
}

func (s *Server) hookLoop() {
	for {
		var j hookJob
		select {
		case <-s.hooks.done:
			return
		case j = <-s.hooks.queue:
		}
		h, err := s.store.GetEventHook(context.Background(), j.hookID)
		if err != nil || !h.Enabled {
			continue // deleted or disabled since
		}
		d := api.HookDelivery{EventID: j.eventID, EventType: j.typ}
		for d.Attempts = 1; d.Attempts <= hookAttempts; d.Attempts++ {
			if d.Attempts > 1 {
				select {
				case <-s.hooks.done:
					return
				case <-time.After(s.hooks.retryDelay(d.Attempts - 1)):
				}
			}
			if err = s.deliverHook(h, j.typ, j.body); err == nil {
				break
			}
		}
		if d.Attempts > hookAttempts {
			d.Attempts = hookAttempts
		}
		d.At, d.OK = s.now(), err == nil
		if err != nil {
			d.Error = err.Error()
		}
		s.recordDelivery(h.ID, d)
	}
}

// deliverHook makes one signed POST.
func (s *Server) deliverHook(h *api.EventHook, typ string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !scopeEmpty(h.ClientScope) {
		ctx = withClientChannel(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "armada-server")
	req.Header.Set("X-Armada-Event", typ)
	req.Header.Set("X-Armada-Hook", h.ID)
	if h.Secret != "" {
		secret, err := s.secrets.open(h.Secret)
		if err != nil {
			return fmt.Errorf("the webhook's secret can't be read: %w", err)
		}
		ts := strconv.FormatInt(s.now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		req.Header.Set("X-Armada-Timestamp", ts)
		req.Header.Set("X-Armada-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	return s.doNotifyRequest(req)
}

// recordDelivery saves a delivery's outcome; too many failures in a row
// disable the webhook.
func (s *Server) recordDelivery(id string, d api.HookDelivery) {
	s.hooks.statusMu.Lock()
	defer s.hooks.statusMu.Unlock()
	ctx := context.Background()
	h, err := s.store.GetEventHook(ctx, id)
	if err != nil {
		return
	}
	h.LastDelivery = &d
	if d.OK {
		h.Failures = 0
	} else {
		h.Failures++
		if h.Failures >= hookMaxFailures && h.Enabled {
			h.Enabled = false
			h.DisabledReason = fmt.Sprintf("disabled after %d failed deliveries in a row; last error: %s", h.Failures, d.Error)
			s.logEvent(ctx, "hook.disabled", "", "", fmt.Sprintf("event webhook %q %s", h.Name, h.DisabledReason))
			s.invalidateHooks()
		} else {
			s.logEvent(ctx, "hook.delivery_failed", "", "", fmt.Sprintf("event webhook %q could not deliver %s #%d: %s", h.Name, d.EventType, d.EventID, d.Error))
		}
	}
	if err := s.store.PutEventHook(ctx, h); err != nil {
		s.log.Error("save webhook delivery", "hook", id, "err", err)
	}
}

// ---- API ----

func redactHook(h *api.EventHook) *api.EventHook {
	c := *h
	c.HasSecret, c.Secret, c.RotateSecret = h.Secret != "", "", false
	return &c
}

func (s *Server) validateHook(h *api.EventHook) error {
	h.Name = strings.TrimSpace(h.Name)
	if h.Name == "" || len(h.Name) > 100 {
		return errors.New("give it a name (up to 100 characters)")
	}
	u, err := url.Parse(strings.TrimSpace(h.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("url must be an http(s):// URL")
	}
	h.URL = u.String()
	if !scopeEmpty(h.ClientScope) {
		if u.Scheme != "https" {
			return errors.New("client webhooks must use an https:// URL")
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && !publicIP(ip) && !s.allowPrivateClientTargets {
			return errNonPublic
		}
	}
	for i, t := range h.Types {
		t = strings.ToLower(strings.TrimSpace(t))
		if !hookTypeRe.MatchString(t) {
			return fmt.Errorf("invalid event type %q (use e.g. device.enrolled or alert.*)", h.Types[i])
		}
		h.Types[i] = t
	}
	return nil
}

func newHookSecret() string { return newSecret("whsec") }

func (s *Server) handleListHooks(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListEventHooks(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := principalFrom(r.Context())
	out := []*api.EventHook{}
	for _, h := range all {
		if see, _ := alertAccess(p, h.ClientScope, true); see {
			out = append(out, redactHook(h))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var h api.EventHook
	if !readJSON(w, r, &h) {
		return
	}
	all, err := s.store.ListEventHooks(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(all) >= maxEventHooks {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d event webhooks", maxEventHooks))
		return
	}
	p := principalFrom(ctx)
	if h.ClientScope, err = pickClientScope(p, h.ClientScope, true, "event webhooks"); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err := s.validateHook(&h); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plain := strings.TrimSpace(h.Secret)
	if plain == "" {
		plain = newHookSecret()
	}
	h.ID, h.Secret, h.CreatedBy, h.CreatedAt = newID("hook"), s.secrets.seal(plain), p.name, s.now()
	h.LastDelivery, h.Failures, h.DisabledReason, h.RotateSecret = nil, 0, "", false
	if err := s.store.PutEventHook(ctx, &h); err != nil {
		s.internalError(w, err)
		return
	}
	s.invalidateHooks()
	s.logEvent(ctx, "hook.created", "", "", fmt.Sprintf("event webhook %q created for %s", h.Name, hookTypesText(&h)))
	out := redactHook(&h)
	out.Secret = plain // shown once
	writeJSON(w, http.StatusCreated, out)
}

func hookTypesText(h *api.EventHook) string {
	if len(h.Types) == 0 {
		return "all events"
	}
	return strings.Join(h.Types, ", ")
}

func (s *Server) loadHook(w http.ResponseWriter, r *http.Request, edit bool) (*api.EventHook, bool) {
	h, err := s.store.GetEventHook(r.Context(), r.PathValue("id"))
	if err == nil {
		see, canEdit := alertAccess(principalFrom(r.Context()), h.ClientScope, true)
		switch {
		case !see:
			err = store.ErrNotFound
		case edit && !canEdit:
			writeError(w, http.StatusForbidden, "you can't change this webhook")
			return nil, false
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event webhook not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return h, true
}

func (s *Server) handleUpdateHook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	old, ok := s.loadHook(w, r, true)
	if !ok {
		return
	}
	var h api.EventHook
	if !readJSON(w, r, &h) {
		return
	}
	h.ID, h.ClientScope, h.CreatedBy, h.CreatedAt, h.LastDelivery = old.ID, old.ClientScope, old.CreatedBy, old.CreatedAt, old.LastDelivery
	if err := s.validateHook(&h); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plain := ""
	switch {
	case h.RotateSecret:
		plain = newHookSecret()
		h.Secret = s.secrets.seal(plain)
	case strings.TrimSpace(h.Secret) != "":
		h.Secret = s.secrets.seal(strings.TrimSpace(h.Secret))
	default:
		h.Secret = old.Secret
	}
	if h.Enabled && !old.Enabled {
		h.Failures, h.DisabledReason = 0, "" // turned back on
	} else {
		h.Failures, h.DisabledReason = old.Failures, old.DisabledReason
	}
	h.RotateSecret = false
	if err := s.store.PutEventHook(ctx, &h); err != nil {
		s.internalError(w, err)
		return
	}
	s.invalidateHooks()
	s.logEvent(ctx, "hook.updated", "", "", fmt.Sprintf("event webhook %q updated", h.Name))
	out := redactHook(&h)
	out.Secret = plain
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteHook(w http.ResponseWriter, r *http.Request) {
	h, ok := s.loadHook(w, r, true)
	if !ok {
		return
	}
	if err := s.store.DeleteEventHook(r.Context(), h.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.invalidateHooks()
	s.logEvent(r.Context(), "hook.deleted", "", "", fmt.Sprintf("event webhook %q deleted", h.Name))
	w.WriteHeader(http.StatusNoContent)
}

// handleTestHook sends a hook.test event now and reports the result.
func (s *Server) handleTestHook(w http.ResponseWriter, r *http.Request) {
	h, ok := s.loadHook(w, r, true)
	if !ok {
		return
	}
	e := &api.Event{Time: s.now(), Type: "hook.test", Message: "test delivery for event webhook " + h.Name, Actor: principalFrom(r.Context()).name}
	body, _ := json.Marshal(hookPayload(h, e, nil))
	err := s.deliverHook(h, e.Type, body)
	res := map[string]any{"ok": err == nil}
	if err != nil {
		res["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}
