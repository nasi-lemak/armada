package server

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestQuickSearch(t *testing.T) {
	_, ts := setup(t)
	enroll := func(name, customer, serial, ip string) string {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": customer}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: name, Info: api.DeviceInfo{OS: "linux", Serial: serial, IPs: []string{ip}}}, &er)
		return er.DeviceID
	}
	a1 := enroll("acme-till-1", "acme", "SN-1001", "10.1.0.11")
	enroll("acme-till-10", "acme", "SN-1010", "10.1.0.20")
	g1 := enroll("globex-kiosk", "globex", "SN-2001", "10.2.0.11")
	call(t, ts, admin, "PATCH", "/api/v1/devices/"+a1, map[string]any{"inventory": api.Inventory{AssetTag: "ACME-00417", Location: "Orchard"}}, nil)
	upload(t, ts, "acmeapp", "2.0.0")
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "it@acme.com", Role: api.RoleOperator,
		Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}, Password: "long-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "long-password-1"}, &lr)

	search := func(token, q string) []api.SearchResult {
		t.Helper()
		var out []api.SearchResult
		if code := call(t, ts, token, "GET", "/api/v1/search?q="+q, nil, &out); code != 200 {
			t.Fatalf("search %q: %d", q, code)
		}
		return out
	}
	for q, want := range map[string]string{"sn-2001": g1, "ACME-00417": a1, "10.1.0.11": a1, "orchard": a1} {
		if r := search(admin, q); len(r) == 0 || r[0].ID != want || r[0].Link != "#/devices/"+want {
			t.Errorf("%s: %+v", q, r)
		}
	}
	// Exact matches first: "acme-till-1" before "acme-till-10".
	if r := search(admin, "acme-till-1"); len(r) < 2 || r[0].ID != a1 {
		t.Fatalf("ranking: %+v", r)
	}
	// Everyone else: releases and users for fleet staff only.
	kinds := func(rs []api.SearchResult) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			m[r.Kind]++
		}
		return m
	}
	if k := kinds(search(admin, "acme")); k["device"] != 2 || k["release"] != 1 || k["user"] != 1 {
		t.Fatalf("admin kinds: %v", k)
	}
	if k := kinds(search(lr.Token, "acme")); k["device"] != 2 || k["release"] != 0 || k["user"] != 0 {
		t.Fatalf("client kinds: %v", k)
	}
	if r := search(lr.Token, "globex"); len(r) != 0 {
		t.Fatalf("client found another client's device: %+v", r)
	}
	if r := search(admin, "a"); len(r) != 0 {
		t.Fatalf("one-letter search: %+v", r)
	}
}
