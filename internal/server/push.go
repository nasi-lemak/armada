package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// pushHub lets the server wake a device immediately. Each agent holds one
// long-poll request (GET /api/v1/agent/wait); waking the device answers that
// request, and the agent checks in at once. This works through NAT, proxies
// and firewalls that only allow outbound HTTPS, and needs no extra port.
type pushHub struct {
	mu      sync.Mutex
	waiters map[string]*waiter
	pending map[string]bool // woken while not connected: answer the next wait at once
	done    chan struct{}
}

type waiter struct {
	wake   chan struct{} // closed to wake
	kicked chan struct{} // closed when a newer wait from the same device replaces this one
}

func newPushHub() *pushHub {
	return &pushHub{waiters: map[string]*waiter{}, pending: map[string]bool{}, done: make(chan struct{})}
}

// subscribe registers a wait for a device. If the device was woken while it
// wasn't connected, ready is true and the caller should return immediately.
func (h *pushHub) subscribe(id string) (w *waiter, ready bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending[id] {
		delete(h.pending, id)
		return nil, true
	}
	if old := h.waiters[id]; old != nil {
		close(old.kicked)
	}
	w = &waiter{wake: make(chan struct{}), kicked: make(chan struct{})}
	h.waiters[id] = w
	return w, false
}

func (h *pushHub) unsubscribe(id string, w *waiter) {
	h.mu.Lock()
	if h.waiters[id] == w {
		delete(h.waiters, id)
	}
	h.mu.Unlock()
}

// wake asks a device to check in now. It reports whether the device was
// connected; if not, the device is told as soon as it reconnects.
func (h *pushHub) wake(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.waiters[id]; w != nil {
		delete(h.waiters, id)
		close(w.wake)
		return true
	}
	h.pending[id] = true
	return false
}

// checkedIn clears a pending wake: the device just reported anyway.
func (h *pushHub) checkedIn(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

func (h *pushHub) connected(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waiters[id] != nil
}

func (h *pushHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

// handleAgentWait holds the request until the device is woken or the hold
// time passes. Answers {"wake": true} when the agent should check in now.
func (s *Server) handleAgentWait(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	hold := s.cfg.PushHold
	if q, err := strconv.Atoi(r.URL.Query().Get("hold")); err == nil && q > 0 && time.Duration(q)*time.Second < hold {
		hold = time.Duration(q) * time.Second
	}
	if hold <= 0 {
		writeError(w, http.StatusNotFound, "push is disabled on this server")
		return
	}
	wt, ready := s.push.subscribe(dev.ID)
	if ready {
		writeJSON(w, http.StatusOK, api.WaitResponse{Wake: true})
		return
	}
	defer s.push.unsubscribe(dev.ID, wt)
	t := time.NewTimer(hold)
	defer t.Stop()
	select {
	case <-wt.wake:
		writeJSON(w, http.StatusOK, api.WaitResponse{Wake: true})
	case <-wt.kicked:
		writeJSON(w, http.StatusOK, api.WaitResponse{})
	case <-t.C:
		writeJSON(w, http.StatusOK, api.WaitResponse{})
	case <-s.push.done:
		writeJSON(w, http.StatusOK, api.WaitResponse{})
	case <-r.Context().Done():
	}
}

// handleWakeDevice asks a device to check in now.
func (s *Server) handleWakeDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok || !needDeviceRole(w, r, d, api.RoleOperator) {
		return
	}
	if d.ParentID != "" {
		d, _ = s.store.GetDevice(r.Context(), d.ParentID)
		if d == nil {
			writeError(w, http.StatusNotFound, "gateway not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, api.WakeResponse{Connected: s.push.wake(d.ID)})
}
