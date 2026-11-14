package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestDeviceTimeline(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	ctx := t.Context()
	enroll := func(name, customer string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux"}}, &er)
		return er.DeviceID
	}
	a1 := enroll("acme-1", "acme")
	enroll("globex-1", "globex")

	// Many entries at one instant, so pages split ties.
	clock = clock.Add(time.Hour)
	for i, typ := range []string{"health.warning", "cert.installed", "custom.thing", "config.applied", "health.ok", "alert.fired", "update.succeeded"} {
		if err := s.store.AddEvent(ctx, api.Event{Time: clock, Type: typ, DeviceID: a1, Message: fmt.Sprintf("tie %d %s", i, typ)}); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(time.Minute)
	call(t, ts, admin, "POST", "/api/v1/devices/"+a1+"/actions", api.CreateActionRequest{Type: "reboot"}, nil)
	acked := clock.Add(5 * time.Minute)
	resolved := clock.Add(30 * time.Minute)
	if err := s.store.PutAlert(ctx, &api.Alert{ID: "al1", RuleID: "r", RuleName: "Disk", Condition: api.ConditionHealth, Key: "k1",
		Status: api.AlertResolved, Severity: "critical", Summary: "disk 99% full", DeviceID: a1, StartedAt: clock, ResolvedAt: &resolved,
		AckedBy: "ops@vendor.example", AckedAt: &acked, AckNote: "ordering a disk", Count: 3, UpdatedAt: resolved}); err != nil {
		t.Fatal(err)
	}
	rel := upload(t, ts, "kiosk", "2.0.0")
	var dep api.Deployment
	call(t, ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID}, &dep)
	if err := s.store.UpsertDeviceUpdate(ctx, &api.DeviceUpdate{DeploymentID: dep.ID, DeviceID: a1, ReleaseID: rel.ID, Package: "kiosk",
		Version: "2.0.0", FromVersion: "1.9.0", Status: api.UpdateFailed, Message: "hook exited 1", CreatedAt: clock, UpdatedAt: clock.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Hour)
	end := clock.Add(2 * time.Hour)
	if code := call(t, ts, admin, "POST", "/api/v1/maintenance", api.Maintenance{Name: "Disk swap", Selector: api.Selector{DeviceIDs: []string{a1}},
		StartsAt: clock, EndsAt: &end}, nil); code != 201 && code != 200 {
		t.Fatalf("maintenance: %d", code)
	}

	get := func(token, q string) (api.Timeline, int) {
		t.Helper()
		var tl api.Timeline
		code := call(t, ts, token, "GET", "/api/v1/devices/"+a1+"/timeline?"+q, nil, &tl)
		return tl, code
	}
	full, _ := get(admin, "limit=200")
	if full.Next != "" {
		t.Fatalf("one page should hold everything: next %q", full.Next)
	}
	byKind := map[string][]api.TimelineEntry{}
	for _, e := range full.Entries {
		byKind[e.Kind] = append(byKind[e.Kind], e)
		if strings.HasPrefix(e.Type, "alert.") || strings.HasPrefix(e.Type, "update.") {
			t.Errorf("alert/update event should come from its record: %+v", e)
		}
	}
	if a := byKind["alert"]; len(a) != 1 || a[0].Tone != "bad" || a[0].End == nil || !strings.Contains(a[0].Detail, "acknowledged by ops@vendor.example: ordering a disk") ||
		!strings.Contains(a[0].Detail, "fired 3 times") {
		t.Fatalf("alert: %+v", a)
	}
	if u := byKind["update"]; len(u) != 1 || u[0].Title != "Update of kiosk from 1.9.0 to 2.0.0 failed" || u[0].Detail != "hook exited 1" || u[0].Link != "#/deployments/"+dep.ID {
		t.Fatalf("update: %+v", u)
	}
	if m := byKind["maintenance"]; len(m) != 1 || m[0].Title != "Maintenance: Disk swap" || m[0].End == nil {
		t.Fatalf("maintenance: %+v", m)
	}
	if a := byKind["action"]; len(a) != 1 || a[0].Actor != "admin token" {
		t.Fatalf("action: %+v", a)
	}
	if len(byKind["health"]) != 2 || len(byKind["cert"]) != 1 || len(byKind["config"]) != 1 || len(byKind["lifecycle"]) != 1 {
		t.Fatalf("kinds: %v", byKind)
	}
	if o := byKind["other"]; len(o) != 1 || o[0].Type != "custom.thing" {
		t.Fatalf("other: %+v", o)
	}
	// Newest first.
	if full.Entries[0].Kind != "maintenance" || full.Entries[len(full.Entries)-1].Type != "device.enrolled" {
		t.Fatalf("order: first %+v last %+v", full.Entries[0], full.Entries[len(full.Entries)-1])
	}

	// Paging two at a time gives the same entries in the same order, ties included.
	for _, size := range []int{1, 2, 3} {
		var paged []string
		cursor := ""
		for i := 0; ; i++ {
			if i > 50 {
				t.Fatal("paging does not end")
			}
			q := url.Values{"limit": {fmt.Sprint(size)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			page, code := get(admin, q.Encode())
			if code != 200 {
				t.Fatalf("page: %d", code)
			}
			for _, e := range page.Entries {
				paged = append(paged, e.ID)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
		var want []string
		for _, e := range full.Entries {
			want = append(want, e.ID)
		}
		if strings.Join(paged, " ") != strings.Join(want, " ") {
			t.Fatalf("size %d:\n got %v\nwant %v", size, paged, want)
		}
	}

	// Kind filters, including "other" with another kind.
	kinds := func(q string) map[string]int {
		tl, code := get(admin, q)
		if code != 200 {
			t.Fatalf("%s: %d", q, code)
		}
		m := map[string]int{}
		for _, e := range tl.Entries {
			m[e.Kind]++
		}
		return m
	}
	if m := kinds("kinds=alert"); len(m) != 1 || m["alert"] != 1 {
		t.Fatalf("alert only: %v", m)
	}
	if m := kinds("kinds=health,other"); len(m) != 2 || m["health"] != 2 || m["other"] != 1 {
		t.Fatalf("health+other: %v", m)
	}
	if _, code := get(admin, "kinds=bogus"); code != 400 {
		t.Fatalf("bad kind: %d", code)
	}
	if _, code := get(admin, "cursor=x"); code != 400 {
		t.Fatalf("bad cursor: %d", code)
	}

	// Another client's account can't read it; the owner's can, without rollout links.
	for _, u := range []struct{ email, customer string }{{"it@globex.com", "globex"}, {"it@acme.com", "acme"}} {
		call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: u.email, Role: api.RoleViewer,
			Scope: api.Selector{Labels: map[string]string{"customer": u.customer}}, Password: "long-password-1"}, nil)
	}
	var lg, la api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@globex.com", Password: "long-password-1"}, &lg)
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &la)
	if _, code := get(lg.Token, ""); code != 404 {
		t.Fatalf("other client: %d", code)
	}
	tl, code := get(la.Token, "kinds=update")
	if code != 200 || len(tl.Entries) != 1 || tl.Entries[0].Link != "" {
		t.Fatalf("own client: %d %+v", code, tl)
	}
}
