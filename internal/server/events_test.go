package server

import (
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestActivityFilters(t *testing.T) {
	_, ts := setup(t)
	enroll := func(name, customer string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er.DeviceID
	}
	a1 := enroll("acme-1", "acme")
	g1 := enroll("globex-1", "globex")
	call(t, ts, admin, "POST", "/api/v1/devices/"+a1+"/actions", api.CreateActionRequest{Type: "reboot"}, nil)
	call(t, ts, admin, "POST", "/api/v1/devices/"+g1+"/actions", api.CreateActionRequest{Type: "reboot"}, nil)
	call(t, ts, admin, "POST", "/api/v1/devices/"+g1+"/lifecycle", api.LifecycleRequest{State: "repair", Note: "cracked_screen 100%"}, nil)
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleViewer,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)

	list := func(token string, q url.Values) []api.Event {
		t.Helper()
		var out []api.Event
		if code := call(t, ts, token, "GET", "/api/v1/events?"+q.Encode(), nil, &out); code != 200 {
			t.Fatalf("events %v: %d", q, code)
		}
		return out
	}
	types := func(evs []api.Event) map[string]int {
		m := map[string]int{}
		for _, e := range evs {
			m[e.Type]++
		}
		return m
	}

	if m := types(list(admin, url.Values{"type": {"action.queued"}})); m["action.queued"] != 2 || len(m) != 1 {
		t.Fatalf("type filter: %v", m)
	}
	if m := types(list(admin, url.Values{"type": {"device.*,user.created"}})); m["device.enrolled"] != 2 || m["user.created"] != 1 || m["action.queued"] != 0 {
		t.Fatalf("prefix filter: %v", m)
	}
	// Enrollment has no signed-in actor; queued actions were the admin token's.
	if m := types(list(admin, url.Values{"actor": {"system"}})); m["device.enrolled"] != 2 || m["action.queued"] != 0 {
		t.Fatalf("system actor: %v", m)
	}
	if m := types(list(admin, url.Values{"actor": {"admin token"}, "device_id": {g1}})); m["action.queued"] != 1 || m["device.enrolled"] != 0 {
		t.Fatalf("actor + device: %v", m)
	}
	// Text search treats % and _ literally.
	if evs := list(admin, url.Values{"q": {"_screen 100%"}}); len(evs) != 1 || evs[0].DeviceID != g1 {
		t.Fatalf("text: %+v", evs)
	}
	if evs := list(admin, url.Values{"q": {"screen 1000"}}); len(evs) != 0 {
		t.Fatalf("text should not match: %+v", evs)
	}
	if evs := list(admin, url.Values{"until": {"2000-01-01"}}); len(evs) != 0 {
		t.Fatalf("until: %+v", evs)
	}
	if code := call(t, ts, admin, "GET", "/api/v1/events?since=yesterday", nil, nil); code != 400 {
		t.Fatalf("bad since: %d", code)
	}

	// Paging with before_id walks the whole log without repeats.
	all := list(admin, url.Values{"limit": {"1000"}})
	seen := map[int64]bool{}
	before := ""
	for {
		q := url.Values{"limit": {"3"}}
		if before != "" {
			q.Set("before_id", before)
		}
		page := list(admin, q)
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			if seen[e.ID] {
				t.Fatalf("event %d repeated", e.ID)
			}
			seen[e.ID] = true
		}
		before = strconv.FormatInt(page[len(page)-1].ID, 10)
	}
	if len(seen) != len(all) {
		t.Fatalf("paging saw %d of %d", len(seen), len(all))
	}

	// A client account sees only events about its own devices, filters or not.
	for _, q := range []url.Values{{}, {"type": {"*"}}, {"device_id": {g1}}, {"q": {"globex"}}} {
		for _, e := range list(lr.Token, q) {
			if e.DeviceID != a1 {
				t.Fatalf("client saw %+v (filter %v)", e, q)
			}
		}
	}
	if m := types(list(lr.Token, url.Values{"type": {"action.queued"}})); m["action.queued"] != 1 {
		t.Fatalf("client actions: %v", m)
	}

	// CSV export.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/events?format=csv&device_id="+g1, nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil || len(rows) < 4 || rows[0][2] != "type" || rows[1][5] != "globex-1" {
		t.Fatalf("csv: %v %q", err, b)
	}

	var ts2 []string
	call(t, ts, lr.Token, "GET", "/api/v1/events/types", nil, &ts2)
	if len(ts2) == 0 {
		t.Fatal("no event types")
	}
}
