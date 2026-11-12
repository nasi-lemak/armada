package server

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/nasi-lemak/armada/internal/api"
)

type scimListResp struct {
	TotalResults int               `json:"totalResults"`
	Resources    []json.RawMessage `json:"Resources"`
}

type scimUserResp struct {
	ID       string `json:"id"`
	UserName string `json:"userName"`
	Active   bool   `json:"active"`
	Groups   []struct {
		Display string `json:"display"`
	} `json:"groups"`
	DisplayName string `json:"displayName"`
}

func patch(ops ...map[string]any) map[string]any {
	return map[string]any{"schemas": []string{scimPatchSchema}, "Operations": ops}
}

func TestSCIMProvisioning(t *testing.T) {
	_, ts := setupWith(t, func(c *Config) {
		c.OIDC.GroupRoles = map[string]string{"Fleet Admins": api.RoleAdmin, "Fleet Ops": api.RoleOperator}
	})
	if c := call(t, ts, admin, "GET", "/scim/v2/Users", nil, nil); c != 401 {
		t.Fatalf("admin token accepted for SCIM: %d", c)
	}
	var tok api.SCIMToken
	call(t, ts, admin, "POST", "/api/v1/scim/tokens", map[string]string{"name": "Entra ID"}, &tok)
	if tok.Token == "" || tok.URL == "" {
		t.Fatalf("token: %+v", tok)
	}
	scim := tok.Token
	if c := call(t, ts, scim, "GET", "/api/v1/devices", nil, nil); c != 401 {
		t.Fatalf("SCIM token reached the admin API: %d", c)
	}
	for _, p := range []string{"/scim/v2/ServiceProviderConfig", "/scim/v2/ResourceTypes", "/scim/v2/Schemas"} {
		if c := call(t, ts, scim, "GET", p, nil, nil); c != 200 {
			t.Errorf("%s: %d", p, c)
		}
	}

	// Entra: look up by userName, then create.
	q := "/scim/v2/Users?filter=" + url.QueryEscape(`userName eq "dana@vendor.com"`)
	var list scimListResp
	call(t, ts, scim, "GET", q, nil, &list)
	if list.TotalResults != 0 {
		t.Fatalf("filter on empty: %+v", list)
	}
	var dana scimUserResp
	c := call(t, ts, scim, "POST", "/scim/v2/Users", map[string]any{
		"schemas": []string{scimUserSchema}, "externalId": "e-123", "userName": "dana@vendor.com", "active": true,
		"displayName": "Dana Lee", "name": map[string]any{"givenName": "Dana", "familyName": "Lee"},
		"emails": []map[string]any{{"value": "dana@vendor.com", "type": "work", "primary": true}},
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": map[string]any{"department": "Ops"},
	}, &dana)
	if c != 201 || dana.ID == "" || !dana.Active || dana.DisplayName != "Dana Lee" {
		t.Fatalf("create: %d %+v", c, dana)
	}
	if c := call(t, ts, scim, "POST", "/scim/v2/Users", map[string]any{"userName": "DANA@vendor.com"}, nil); c != 409 {
		t.Fatalf("duplicate: %d", c)
	}
	call(t, ts, scim, "GET", q, nil, &list)
	if list.TotalResults != 1 {
		t.Fatalf("filter: %+v", list)
	}
	call(t, ts, scim, "GET", "/scim/v2/Users?filter="+url.QueryEscape(`externalId eq "e-123"`), nil, &list)
	if list.TotalResults != 1 {
		t.Fatal("externalId filter")
	}
	if c := call(t, ts, scim, "GET", "/scim/v2/Users?filter="+url.QueryEscape(`userName sw "d"`), nil, nil); c != 400 {
		t.Fatalf("unsupported filter: %d", c)
	}
	var users []api.User
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	if len(users) != 1 || users[0].Role != api.RoleViewer || !users[0].NoPassword || !users[0].Provisioned {
		t.Fatalf("provisioned user: %+v", users)
	}

	// Groups set roles; leaving the group takes the role away again.
	var ops struct {
		ID string `json:"id"`
	}
	call(t, ts, scim, "POST", "/scim/v2/Groups", map[string]any{"schemas": []string{scimGroupSchema}, "displayName": "Fleet Ops",
		"members": []map[string]any{{"value": dana.ID}}}, &ops)
	role := func() string {
		var u []api.User
		call(t, ts, admin, "GET", "/api/v1/users", nil, &u)
		return u[0].Role
	}
	if r := role(); r != api.RoleOperator {
		t.Fatalf("role from group: %s", r)
	}
	var admins struct {
		ID string `json:"id"`
	}
	call(t, ts, scim, "POST", "/scim/v2/Groups", map[string]any{"displayName": "fleet admins"}, &admins)
	call(t, ts, scim, "PATCH", "/scim/v2/Groups/"+admins.ID, patch(map[string]any{"op": "Add", "path": "members", "value": []map[string]any{{"value": dana.ID}}}), nil)
	if r := role(); r != api.RoleAdmin {
		t.Fatalf("highest group wins: %s", r)
	}
	// Admins can't change a directory-managed role by hand.
	if c := call(t, ts, admin, "PATCH", "/api/v1/users/"+dana.ID, map[string]any{"role": "viewer"}, nil); c != 400 {
		t.Fatalf("manual role change: %d", c)
	}
	// Entra removes a member with a filter path.
	call(t, ts, scim, "PATCH", "/scim/v2/Groups/"+admins.ID, patch(map[string]any{"op": "Remove", "path": `members[value eq "` + dana.ID + `"]`}), nil)
	if r := role(); r != api.RoleOperator {
		t.Fatalf("after leaving admins: %s", r)
	}
	call(t, ts, scim, "DELETE", "/scim/v2/Groups/"+ops.ID, nil, nil)
	if r := role(); r != api.RoleViewer {
		t.Fatalf("after group deleted: %s", r)
	}
	var grp struct {
		Members []struct{ Value string } `json:"members"`
	}
	call(t, ts, scim, "PATCH", "/scim/v2/Groups/"+admins.ID, patch(map[string]any{"op": "replace", "value": map[string]any{"members": []map[string]any{{"value": dana.ID}, {"value": "usr_nobody"}}}}), &grp)
	if len(grp.Members) != 1 {
		t.Fatalf("unknown member kept: %+v", grp)
	}

	// Deactivation (Entra sends the string "False") signs the user out.
	var sess api.LoginResponse
	call(t, ts, admin, "PATCH", "/api/v1/users/"+dana.ID, map[string]any{"password": "temporary-password"}, nil)
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "dana@vendor.com", Password: "temporary-password"}, &sess)
	call(t, ts, scim, "PATCH", "/scim/v2/Users/"+dana.ID, patch(
		map[string]any{"op": "Replace", "path": "active", "value": "False"},
		map[string]any{"op": "Replace", "path": `emails[type eq "work"].value`, "value": "dana.lee@vendor.com"},
		map[string]any{"op": "Add", "path": "title", "value": "Engineer"},
	), &dana)
	if dana.Active || dana.UserName != "dana.lee@vendor.com" {
		t.Fatalf("patch: %+v", dana)
	}
	if c := call(t, ts, sess.Token, "GET", "/api/v1/me", nil, nil); c != 401 {
		t.Fatalf("deactivated user still signed in: %d", c)
	}
	// Okta reactivates with a path-less replace.
	call(t, ts, scim, "PATCH", "/scim/v2/Users/"+dana.ID, patch(map[string]any{"op": "replace", "value": map[string]any{"active": true, "displayName": "Dana L."}}), &dana)
	if !dana.Active || dana.DisplayName != "Dana L." || len(dana.Groups) != 1 {
		t.Fatalf("reactivate: %+v", dana)
	}
	// PUT replaces the user.
	call(t, ts, scim, "PUT", "/scim/v2/Users/"+dana.ID, map[string]any{"userName": "dana.lee@vendor.com", "name": map[string]any{"formatted": "Dana Lee"}, "active": true}, &dana)
	if dana.DisplayName != "Dana Lee" {
		t.Fatalf("put: %+v", dana)
	}

	// An existing hand-made account becomes directory-managed when the
	// directory touches it, and its role then follows groups.
	var manual api.User
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "sam@vendor.com", Role: api.RoleAdmin, Password: "sam-password-1"}, &manual)
	call(t, ts, scim, "PATCH", "/scim/v2/Users/"+manual.ID, patch(map[string]any{"op": "replace", "path": "externalId", "value": "e-9"}), nil)
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	for _, u := range users {
		if u.ID == manual.ID && (u.Role != api.RoleViewer || !u.Provisioned || u.NoPassword) {
			t.Fatalf("matched account: %+v", u)
		}
	}

	// Paging and deletion.
	call(t, ts, scim, "GET", "/scim/v2/Users?startIndex=2&count=1", nil, &list)
	if list.TotalResults != 2 || len(list.Resources) != 1 {
		t.Fatalf("paging: %+v", list)
	}
	if c := call(t, ts, scim, "DELETE", "/scim/v2/Users/"+dana.ID, nil, nil); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	if c := call(t, ts, scim, "GET", "/scim/v2/Users/"+dana.ID, nil, nil); c != 404 {
		t.Fatalf("deleted user: %d", c)
	}
	// Revoked tokens stop working.
	call(t, ts, admin, "DELETE", "/api/v1/scim/tokens/"+tok.ID, nil, nil)
	if c := call(t, ts, scim, "GET", "/scim/v2/Users", nil, nil); c != 401 {
		t.Fatalf("revoked token: %d", c)
	}
}
