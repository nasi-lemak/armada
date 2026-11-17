package server

import (
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestActivityLogIntegrity(t *testing.T) {
	s, ts := setup(t)
	for _, name := range []string{"a", "b", "c"} {
		s.event(t.Context(), "test.happened", "", "", name)
	}
	var st api.EventChainStatus
	if code := call(t, ts, admin, "GET", "/api/v1/events/integrity", nil, &st); code != 200 || !st.Intact || st.Checked < 3 || st.HeadHash == "" {
		t.Fatalf("intact log: %d %+v", code, st)
	}
	// Events carry their hash, so a SIEM receiving them can follow the chain.
	var evs []api.Event
	call(t, ts, admin, "GET", "/api/v1/events?limit=1", nil, &evs)
	if len(evs) != 1 || evs[0].Hash != st.HeadHash {
		t.Errorf("newest event %+v, head %s", evs, st.HeadHash)
	}

	// Someone edits the database behind Armada's back.
	if _, err := s.store.DB().Exec(`UPDATE events SET message = 'nothing to see' WHERE id = (SELECT MIN(id) FROM events)`); err != nil {
		t.Fatal(err)
	}
	// GET returns the last check; POST checks again.
	var cached api.EventChainStatus
	call(t, ts, admin, "GET", "/api/v1/events/integrity", nil, &cached)
	if !cached.Intact {
		t.Error("GET re-checked instead of returning the last check")
	}
	call(t, ts, admin, "POST", "/api/v1/events/integrity", nil, &st)
	if st.Intact || st.BrokenAtID == 0 {
		t.Fatalf("tampering not found: %+v", st)
	}
	call(t, ts, admin, "GET", "/api/v1/events?type=activity_log.*", nil, &evs)
	if len(evs) != 1 || !strings.Contains(evs[0].Message, "outside Armada") {
		t.Errorf("no activity_log.broken event: %+v", evs)
	}
	// Checking again doesn't repeat the event.
	call(t, ts, admin, "POST", "/api/v1/events/integrity", nil, &st)
	call(t, ts, admin, "GET", "/api/v1/events?type=activity_log.*", nil, &evs)
	if len(evs) != 1 {
		t.Errorf("%d activity_log.broken events after a second check", len(evs))
	}
	if _, body := getMetrics(t, ts.URL, admin); !strings.Contains(body, "armada_activity_log_intact 0\n") {
		t.Error("metrics don't show the broken chain")
	}

	// Only fleet admins may check.
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "ops@vendor.com", Role: api.RoleOperator, Password: "operator-password"}, nil)
	var login api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "ops@vendor.com", Password: "operator-password"}, &login)
	if code := call(t, ts, login.Token, "GET", "/api/v1/events/integrity", nil, nil); code != 403 {
		t.Errorf("operator: %d", code)
	}
}
