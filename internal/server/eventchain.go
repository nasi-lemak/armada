package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// The activity log's hash chain (store.VerifyEvents) is checked when the
// server starts, once a day, and on request. A broken chain is logged, and
// recorded as an activity_log.broken event, which event webhooks send on.

const eventChainEvery = 24 * time.Hour

type eventChainState struct {
	mu   sync.Mutex
	last *api.EventChainStatus
	run  sync.Mutex // one check at a time
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// checkEventChain verifies the chain now and remembers the result.
func (s *Server) checkEventChain(ctx context.Context) (*api.EventChainStatus, error) {
	s.eventChain.run.Lock()
	defer s.eventChain.run.Unlock()
	st, err := s.store.VerifyEvents(ctx)
	if err != nil {
		return nil, err
	}
	s.eventChain.mu.Lock()
	prev := s.eventChain.last
	s.eventChain.last = st
	s.eventChain.mu.Unlock()
	if !st.Intact {
		s.log.Error("activity log hash chain is broken: events were changed outside Armada", "problem", st.Problem, "event", st.BrokenAtID)
		if prev == nil || prev.Intact || prev.BrokenAtID != st.BrokenAtID {
			s.event(ctx, "activity_log.broken", "", "", st.Problem)
		}
	}
	return st, nil
}

// eventChainTick runs the daily check.
func (s *Server) eventChainTick(ctx context.Context) {
	s.eventChain.mu.Lock()
	last := s.eventChain.last
	s.eventChain.mu.Unlock()
	if last != nil && time.Since(last.CheckedAt) < eventChainEvery {
		return
	}
	if _, err := s.checkEventChain(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("checking the activity log hash chain failed", "err", err)
	}
}

func (s *Server) lastEventChain() *api.EventChainStatus {
	s.eventChain.mu.Lock()
	defer s.eventChain.mu.Unlock()
	return s.eventChain.last
}

// handleEventIntegrity returns the last check, making one if there's none.
func (s *Server) handleEventIntegrity(w http.ResponseWriter, r *http.Request) {
	s.eventIntegrity(w, r, false)
}

// handleCheckEventIntegrity checks the chain now.
func (s *Server) handleCheckEventIntegrity(w http.ResponseWriter, r *http.Request) {
	s.eventIntegrity(w, r, true)
}

func (s *Server) eventIntegrity(w http.ResponseWriter, r *http.Request, now bool) {
	st := s.lastEventChain()
	if st == nil || now {
		var err error
		if st, err = s.checkEventChain(r.Context()); err != nil {
			s.internalError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, st)
}
