package server

import (
	"strings"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

func TestGroupScopesForClientAccounts(t *testing.T) {
	idp := newFakeIdP(t)
	s, ts := setupWith(t, func(c *Config) {
		c.OIDC = OIDCConfig{Issuer: idp.srv.URL, ClientID: "armada-test", GroupsClaim: "groups", CreateRole: api.RoleViewer}
	})
	s.cfg.Android.PublicURL = ts.URL
	// Devices for two customers.
	devs := map[string]string{}
	for _, c := range []string{"acme", "globex", "initech"} {
		var tok api.EnrollmentToken
		call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": c}}, &tok)
		var er api.EnrollResponse
		call(t, ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: tok.Token, Name: c + "-1", Info: api.DeviceInfo{OS: "linux"}}, &er)
		devs[c] = er.DeviceID
	}
	mapGroup := func(group, role string, labels map[string]string) (api.GroupMapping, int) {
		var m api.GroupMapping
		c := call(t, ts, admin, "POST", "/api/v1/group-mappings", api.GroupMapping{Group: group, Role: role, Scope: api.Selector{Labels: labels}}, &m)
		return m, c
	}
	acmeMap, _ := mapGroup("Acme IT", api.RoleOperator, map[string]string{"customer": "acme"})
	mapGroup("Globex Viewers", api.RoleViewer, map[string]string{"customer": "globex"})
	mapGroup("Fleet Ops", api.RoleOperator, nil)
	if _, c := mapGroup("acme it", api.RoleViewer, nil); c != 409 {
		t.Fatalf("duplicate group (case-insensitive): %d", c)
	}
	if _, c := mapGroup("Acme Admins", api.RoleAdmin, map[string]string{"customer": "acme"}); c != 400 {
		t.Fatalf("scoped admin mapping: %d", c)
	}

	// SCIM pushes a client's IT person in "Acme IT".
	var tok api.SCIMToken
	call(t, ts, admin, "POST", "/api/v1/scim/tokens", map[string]string{"name": "Okta"}, &tok)
	var u struct{ ID string }
	call(t, ts, tok.Token, "POST", "/scim/v2/Users", map[string]any{"userName": "it@acme.com", "active": true}, &u)
	var acmeGrp, globexGrp, opsGrp struct{ ID string }
	call(t, ts, tok.Token, "POST", "/scim/v2/Groups", map[string]any{"displayName": "Acme IT", "members": []map[string]any{{"value": u.ID}}}, &acmeGrp)
	call(t, ts, tok.Token, "POST", "/scim/v2/Groups", map[string]any{"displayName": "Globex Viewers"}, &globexGrp)
	call(t, ts, tok.Token, "POST", "/scim/v2/Groups", map[string]any{"displayName": "Fleet Ops"}, &opsGrp)

	// Sign in (password set by an admin just for the test; normally SSO).
	call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"password": "client-password-1"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "client-password-1"}, &lr)
	sees := func() []string {
		var ds []api.Device
		call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, &ds)
		var out []string
		for _, d := range ds {
			out = append(out, d.Labels["customer"])
		}
		return out
	}
	var me api.Me
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.Role != api.RoleOperator || !me.Scoped || len(me.Scopes) != 1 {
		t.Fatalf("acme IT: %+v", me)
	}
	if got := sees(); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("acme IT sees %v", got)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices/"+devs["globex"], nil, nil); c != 404 {
		t.Fatalf("other customer's device: %d", c)
	}
	// Admins can't override directory-managed access by hand.
	if c := call(t, ts, admin, "PATCH", "/api/v1/users/"+u.ID, map[string]any{"scope": api.Selector{}}, nil); c != 400 {
		t.Fatalf("manual scope change: %d", c)
	}

	// In a second client's group: both clients' devices, each with its own role.
	call(t, ts, tok.Token, "PATCH", "/scim/v2/Groups/"+globexGrp.ID, patch(map[string]any{"op": "add", "path": "members", "value": []map[string]any{{"value": u.ID}}}), nil)
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if got := strings.Join(sees(), ","); got != "acme,globex" && got != "globex,acme" {
		t.Fatalf("two clients: sees %s", got)
	}
	if len(me.Grants) != 2 || me.FleetRole != "" {
		t.Fatalf("per-client grants: %+v", me)
	}
	if c := call(t, ts, lr.Token, "POST", "/api/v1/devices/"+devs["acme"]+"/actions", api.CreateActionRequest{Type: "reboot"}, nil); c != 201 {
		t.Fatalf("operator on acme: %d", c)
	}
	if c := call(t, ts, lr.Token, "POST", "/api/v1/devices/"+devs["globex"]+"/actions", api.CreateActionRequest{Type: "reboot"}, nil); c != 403 {
		t.Fatalf("viewer on globex rebooted it: %d", c)
	}
	if c := call(t, ts, lr.Token, "PATCH", "/api/v1/devices/"+devs["globex"], map[string]any{"name": "x"}, nil); c != 403 {
		t.Fatalf("viewer on globex renamed it: %d", c)
	}
	// Enrollment tokens go where they are an operator (only acme here).
	var et0 api.EnrollmentToken
	if c := call(t, ts, lr.Token, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, &et0); c != 201 || et0.Labels["customer"] != "acme" {
		t.Fatalf("token: %d %+v", c, et0)
	}

	// Fleet-wide group wins: internal staff see everything.
	call(t, ts, tok.Token, "PATCH", "/scim/v2/Groups/"+opsGrp.ID, patch(map[string]any{"op": "add", "path": "members", "value": []map[string]any{{"value": u.ID}}}), nil)
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.Scoped || me.Role != api.RoleOperator || len(sees()) != 3 {
		t.Fatalf("fleet-wide group: %+v", me)
	}
	call(t, ts, tok.Token, "PATCH", "/scim/v2/Groups/"+opsGrp.ID, patch(map[string]any{"op": "remove", "path": `members[value eq "` + u.ID + `"]`}), nil)
	call(t, ts, tok.Token, "PATCH", "/scim/v2/Groups/"+globexGrp.ID, patch(map[string]any{"op": "remove", "path": `members[value eq "` + u.ID + `"]`}), nil)

	// Changing a mapping applies at once.
	acmeMap.Scope = api.Selector{Labels: map[string]string{"customer": "initech"}}
	call(t, ts, admin, "PUT", "/api/v1/group-mappings/"+acmeMap.ID, acmeMap, nil)
	if got := sees(); len(got) != 1 || got[0] != "initech" {
		t.Fatalf("after mapping change: %v", got)
	}
	// Out of every mapped group: no access, even with a live session.
	call(t, ts, tok.Token, "PATCH", "/scim/v2/Groups/"+acmeGrp.ID, patch(map[string]any{"op": "replace", "path": "members", "value": []map[string]any{}}), nil)
	if c := call(t, ts, lr.Token, "GET", "/api/v1/me", nil, nil); c != 401 {
		t.Fatalf("no mapped group but still in: %d", c)
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "it@acme.com", Password: "client-password-1"}, nil); c != 403 {
		t.Fatalf("sign-in with no mapped group: %d", c)
	}

	// SSO group claims work the same way, for users not managed by SCIM.
	idp.as(map[string]any{"sub": "g1", "email": "ops@globex.com", "groups": []string{"Globex Viewers", "Staff"}})
	ticket, msg := ssoSignIn(t, ts, true)
	if ticket == "" {
		t.Fatalf("SSO with client group: %s", msg)
	}
	call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, &lr)
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.Role != api.RoleViewer || len(me.Scopes) != 1 || me.Scopes[0].Labels["customer"] != "globex" {
		t.Fatalf("SSO client: %+v", me)
	}
	idp.as(map[string]any{"sub": "g2", "email": "temp@globex.com", "groups": []string{"Staff"}})
	if _, msg := ssoSignIn(t, ts, true); !strings.Contains(msg, "group allowed") {
		t.Fatalf("SSO without mapped group: %q", msg)
	}

	// A multi-client operator must pick a client for enrollment tokens.
	call(t, ts, admin, "PUT", "/api/v1/group-mappings/"+acmeMap.ID, api.GroupMapping{Group: "Acme IT", Role: api.RoleOperator, Scope: api.Selector{Labels: map[string]string{"customer": "acme"}}}, nil)
	mapGroup("Globex Ops", api.RoleOperator, map[string]string{"customer": "globex"})
	idp.as(map[string]any{"sub": "p1", "email": "partner@msp.com", "groups": []string{"Acme IT", "Globex Ops"}})
	ticket, _ = ssoSignIn(t, ts, true)
	call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, &lr)
	if c := call(t, ts, lr.Token, "POST", "/api/v1/tokens", api.CreateTokenRequest{}, nil); c != 400 {
		t.Fatalf("ambiguous token: %d", c)
	}
	var et api.EnrollmentToken
	if c := call(t, ts, lr.Token, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "globex", "site": "hq"}}, &et); c != 201 || et.Labels["customer"] != "globex" {
		t.Fatalf("token for one client: %d %+v", c, et)
	}

	// Removing every mapping must not widen access: group-managed users get
	// none until an admin sets it by hand.
	var ms []api.GroupMapping
	call(t, ts, admin, "GET", "/api/v1/group-mappings", nil, &ms)
	for _, m := range ms {
		call(t, ts, admin, "DELETE", "/api/v1/group-mappings/"+m.ID, nil, nil)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, nil); c != 401 {
		t.Fatalf("client account after mappings removed: %d", c)
	}
	var users []api.User
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	for _, x := range users {
		if x.Email == "partner@msp.com" {
			call(t, ts, admin, "PATCH", "/api/v1/users/"+x.ID, map[string]any{"role": "viewer", "scope": api.Selector{Labels: map[string]string{"customer": "acme"}}}, nil)
		}
	}
	var ds []api.Device
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, &ds); c != 200 || len(ds) != 1 {
		t.Fatalf("after manual access: %d %d", c, len(ds))
	}
}
