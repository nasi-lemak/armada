package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// SCIM 2.0 (RFC 7643/7644) lets an identity provider (Okta, Entra ID,
// OneLogin, JumpCloud…) create, update and deactivate Armada users, and push
// groups. Groups named in the group-to-role mapping set provisioned users'
// roles; deactivating a user in the directory disables them here at once.

const (
	scimUserSchema  = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimGroupSchema = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimListSchema  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimErrSchema   = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimPatchSchema = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

func scimJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func scimError(w http.ResponseWriter, code int, scimType, detail string) {
	body := map[string]any{"schemas": []string{scimErrSchema}, "status": strconv.Itoa(code), "detail": detail}
	if scimType != "" {
		body["scimType"] = scimType
	}
	scimJSON(w, code, body)
}

// scimAuth checks the SCIM bearer token.
func (s *Server) scimAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if !strings.HasPrefix(tok, "scim_") {
			scimError(w, http.StatusUnauthorized, "", "a SCIM token is required (create one under Users in the dashboard)")
			return
		}
		t, err := s.store.SCIMTokenByHash(r.Context(), hashSecret(tok), s.now())
		if err != nil {
			scimError(w, http.StatusUnauthorized, "", "invalid SCIM token")
			return
		}
		p := &principal{name: "SCIM (" + t.Name + ")", role: api.RoleAdmin, method: "scim"}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

func (s *Server) scimBase(r *http.Request) string { return s.publicURL(r) + "/scim/v2" }

// ---- resources ----

type scimName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimMulti struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
	Ref     string `json:"$ref,omitempty"`
}

type scimMeta struct {
	ResourceType string    `json:"resourceType"`
	Created      time.Time `json:"created"`
	LastModified time.Time `json:"lastModified"`
	Location     string    `json:"location"`
}

type scimUser struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id"`
	ExternalID  string      `json:"externalId,omitempty"`
	UserName    string      `json:"userName"`
	Name        scimName    `json:"name"`
	DisplayName string      `json:"displayName,omitempty"`
	Emails      []scimMulti `json:"emails"`
	Active      bool        `json:"active"`
	Groups      []scimMulti `json:"groups"`
	Meta        scimMeta    `json:"meta"`
}

type scimGroupRes struct {
	Schemas     []string    `json:"schemas"`
	ID          string      `json:"id"`
	ExternalID  string      `json:"externalId,omitempty"`
	DisplayName string      `json:"displayName"`
	Members     []scimMulti `json:"members,omitempty"`
	Meta        scimMeta    `json:"meta"`
}

func (s *Server) userResource(r *http.Request, u *api.User, groups []*api.SCIMGroup) scimUser {
	updated := u.CreatedAt
	if u.UpdatedAt != nil {
		updated = *u.UpdatedAt
	}
	res := scimUser{Schemas: []string{scimUserSchema}, ID: u.ID, ExternalID: u.ExternalID, UserName: u.Email,
		Name: scimName{Formatted: u.Name}, DisplayName: u.Name, Active: !u.Disabled,
		Emails: []scimMulti{{Value: u.Email, Type: "work", Primary: true}}, Groups: []scimMulti{},
		Meta: scimMeta{ResourceType: "User", Created: u.CreatedAt, LastModified: updated, Location: s.scimBase(r) + "/Users/" + u.ID}}
	for _, g := range groups {
		for _, m := range g.Members {
			if m == u.ID {
				res.Groups = append(res.Groups, scimMulti{Value: g.ID, Display: g.DisplayName, Ref: s.scimBase(r) + "/Groups/" + g.ID})
			}
		}
	}
	return res
}

func (s *Server) groupResource(r *http.Request, g *api.SCIMGroup, users map[string]*api.User, withMembers bool) scimGroupRes {
	res := scimGroupRes{Schemas: []string{scimGroupSchema}, ID: g.ID, ExternalID: g.ExternalID, DisplayName: g.DisplayName,
		Meta: scimMeta{ResourceType: "Group", Created: g.CreatedAt, LastModified: g.UpdatedAt, Location: s.scimBase(r) + "/Groups/" + g.ID}}
	if withMembers {
		res.Members = []scimMulti{}
		for _, m := range g.Members {
			if u := users[m]; u != nil {
				res.Members = append(res.Members, scimMulti{Value: m, Display: u.Email, Ref: s.scimBase(r) + "/Users/" + m})
			}
		}
	}
	return res
}

func scimList(resources []any, start, total int) map[string]any {
	return map[string]any{"schemas": []string{scimListSchema}, "totalResults": total, "startIndex": start,
		"itemsPerPage": len(resources), "Resources": resources}
}

// ---- filters and paging ----

var scimFilterRe = regexp.MustCompile(`(?i)^\s*([A-Za-z0-9.:\[\]" ]+?)\s+eq\s+"((?:[^"\\]|\\.)*)"\s*$`)

// scimFilter parses the only filter identity providers need: attr eq "value".
func scimFilter(f string) (attr, value string, err error) {
	if strings.TrimSpace(f) == "" {
		return "", "", nil
	}
	m := scimFilterRe.FindStringSubmatch(f)
	if m == nil {
		return "", "", errors.New(`only filters of the form attribute eq "value" are supported`)
	}
	attr = strings.ToLower(strings.ReplaceAll(m[1], " ", ""))
	if i := strings.LastIndex(attr, ":"); i >= 0 && strings.HasPrefix(attr, "urn:") {
		attr = attr[i+1:] // urn:ietf:params:scim:schemas:core:2.0:User:userName
	}
	value = strings.ReplaceAll(strings.ReplaceAll(m[2], `\"`, `"`), `\\`, `\`)
	return attr, value, nil
}

func scimPage(r *http.Request, n int) (start, end int) {
	q := r.URL.Query()
	start, _ = strconv.Atoi(q.Get("startIndex"))
	if start < 1 {
		start = 1
	}
	count, err := strconv.Atoi(q.Get("count"))
	if err != nil || count < 0 {
		count = 100
	}
	count = min(count, 1000)
	from := min(start-1, n)
	return start, min(from+count, n)
}

// ---- users ----

func (s *Server) handleSCIMListUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attr, value, err := scimFilter(r.URL.Query().Get("filter"))
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	groups, err := s.store.ListSCIMGroups(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var match []*api.User
	for _, u := range users {
		switch attr {
		case "":
		case "username", "emails.value", `emails[type eq "work"].value`, "emails[typeeq\"work\"].value":
			if !strings.EqualFold(u.Email, value) {
				continue
			}
		case "externalid":
			if u.ExternalID != value {
				continue
			}
		case "id":
			if u.ID != value {
				continue
			}
		default:
			scimError(w, http.StatusBadRequest, "invalidFilter", "cannot filter users by "+attr)
			return
		}
		match = append(match, u)
	}
	start, end := scimPage(r, len(match))
	res := []any{}
	for _, u := range match[min(start-1, len(match)):end] {
		res = append(res, s.userResource(r, u, groups))
	}
	scimJSON(w, http.StatusOK, scimList(res, start, len(match)))
}

func (s *Server) handleSCIMGetUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		scimError(w, http.StatusNotFound, "", "user not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	groups, err := s.store.ListSCIMGroups(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	scimJSON(w, http.StatusOK, s.userResource(r, u, groups))
}

// flexBool accepts true/false and the strings "True"/"False" some
// providers send.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case bool:
		*b = flexBool(x)
	case string:
		p, err := strconv.ParseBool(strings.TrimSpace(x))
		if err != nil {
			return fmt.Errorf("invalid boolean %q", x)
		}
		*b = flexBool(p)
	default:
		return errors.New("invalid boolean")
	}
	return nil
}

// userInput is what a POST or PUT carries.
type userInput struct {
	ExternalID  *string     `json:"externalId"`
	UserName    string      `json:"userName"`
	Name        *scimName   `json:"name"`
	DisplayName *string     `json:"displayName"`
	Emails      []scimMulti `json:"emails"`
	Active      *flexBool   `json:"active"`
}

// email picks the sign-in email: userName if it is one, else the primary
// (or work, or first) email.
func (in *userInput) email() string {
	if a, err := mail.ParseAddress(in.UserName); err == nil && a.Address == in.UserName {
		return strings.ToLower(in.UserName)
	}
	best := ""
	for _, e := range in.Emails {
		if e.Primary {
			return strings.ToLower(e.Value)
		}
		if best == "" || e.Type == "work" {
			best = e.Value
		}
	}
	return strings.ToLower(best)
}

func (in *userInput) name() string {
	if in.DisplayName != nil && *in.DisplayName != "" {
		return *in.DisplayName
	}
	if in.Name != nil {
		if in.Name.Formatted != "" {
			return in.Name.Formatted
		}
		return strings.TrimSpace(in.Name.GivenName + " " + in.Name.FamilyName)
	}
	return ""
}

func (s *Server) handleSCIMCreateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in userInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	email := in.email()
	if _, err := mail.ParseAddress(email); err != nil || email == "" {
		scimError(w, http.StatusBadRequest, "invalidValue", "userName or a primary email must be an email address")
		return
	}
	now := s.now()
	u := &api.User{ID: newID("usr"), Email: email, Name: truncate(in.name(), 128), Role: s.cfg.SCIMDefaultRole,
		NoPassword: true, Provisioned: true, CreatedAt: now, UpdatedAt: &now}
	if in.ExternalID != nil {
		u.ExternalID = truncate(*in.ExternalID, 256)
	}
	if in.Active != nil && !*in.Active {
		u.Disabled = true
	}
	if err := s.store.CreateUser(ctx, u, ""); errors.Is(err, store.ErrConflict) {
		scimError(w, http.StatusConflict, "uniqueness", "a user with this email already exists")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.created", "", "", fmt.Sprintf("user %s provisioned from the directory", u.Email))
	if err := s.refreshGroupAccess(ctx, []string{u.ID}); err != nil {
		s.internalError(w, err)
		return
	}
	scimJSON(w, http.StatusCreated, s.userResource(r, u, nil))
}

// handleSCIMReplaceUser is PUT: the provider sends the whole user.
func (s *Server) handleSCIMReplaceUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.scimLoadUser(w, r)
	if !ok {
		return
	}
	var in userInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	next := *u
	if e := in.email(); e != "" {
		next.Email = e
	}
	next.Name = truncate(in.name(), 128)
	if in.ExternalID != nil {
		next.ExternalID = truncate(*in.ExternalID, 256)
	}
	if in.Active != nil {
		next.Disabled = !bool(*in.Active)
	}
	s.scimSaveUser(w, r, u, &next)
}

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

type patchRequest struct {
	Operations []patchOp `json:"Operations"`
}

// userPatchState is the editable part of a user during a PATCH.
type userPatchState struct {
	email, display, formatted, given, family, externalID string
	active                                               bool
}

func (st *userPatchState) set(path string, raw json.RawMessage, remove bool) error {
	str := func() (string, error) {
		if remove {
			return "", nil
		}
		var v string
		return v, json.Unmarshal(raw, &v)
	}
	var err error
	switch strings.ToLower(strings.ReplaceAll(path, " ", "")) {
	case "active":
		if remove {
			return nil
		}
		var b flexBool
		if err = json.Unmarshal(raw, &b); err == nil {
			st.active = bool(b)
		}
	case "username", `emails[type eq"work"].value`, `emails[typeeq"work"].value`, "emails.value":
		var v string
		if v, err = str(); err == nil && v != "" {
			if strings.EqualFold(path, "username") {
				if _, perr := mail.ParseAddress(v); perr != nil {
					return nil // a non-email userName (e.g. a UPN); the email attribute decides
				}
			}
			st.email = strings.ToLower(v)
		}
	case "displayname":
		st.display, err = str()
	case "name.formatted":
		st.formatted, err = str()
	case "name.givenname":
		st.given, err = str()
	case "name.familyname":
		st.family, err = str()
	case "name":
		var n scimName
		if !remove {
			err = json.Unmarshal(raw, &n)
		}
		st.formatted, st.given, st.family = n.Formatted, n.GivenName, n.FamilyName
	case "externalid":
		st.externalID, err = str()
	case "emails":
		var es []scimMulti
		if !remove && json.Unmarshal(raw, &es) == nil {
			in := userInput{Emails: es}
			if e := in.email(); e != "" {
				st.email = e
			}
		}
	default:
		// Titles, phone numbers, enterprise extension attributes and the
		// like aren't used by Armada; accept and ignore them.
	}
	return err
}

func (s *Server) handleSCIMPatchUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.scimLoadUser(w, r)
	if !ok {
		return
	}
	var req patchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	st := userPatchState{email: u.Email, display: u.Name, externalID: u.ExternalID, active: !u.Disabled}
	for _, op := range req.Operations {
		kind := strings.ToLower(op.Op)
		if kind != "add" && kind != "replace" && kind != "remove" {
			scimError(w, http.StatusBadRequest, "invalidSyntax", "unknown op "+op.Op)
			return
		}
		if op.Path == "" {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &obj); err != nil {
				scimError(w, http.StatusBadRequest, "invalidValue", "an operation without a path needs an object value")
				return
			}
			for k, v := range obj {
				if err := st.set(k, v, false); err != nil {
					scimError(w, http.StatusBadRequest, "invalidValue", k+": "+err.Error())
					return
				}
			}
			continue
		}
		if err := st.set(op.Path, op.Value, kind == "remove"); err != nil {
			scimError(w, http.StatusBadRequest, "invalidValue", op.Path+": "+err.Error())
			return
		}
	}
	next := *u
	next.Email, next.ExternalID, next.Disabled = st.email, truncate(st.externalID, 256), !st.active
	switch {
	case st.display != "" && st.display != u.Name:
		next.Name = st.display
	case st.formatted != "":
		next.Name = st.formatted
	case st.given != "" || st.family != "":
		next.Name = strings.TrimSpace(st.given + " " + st.family)
	default:
		next.Name = st.display
	}
	next.Name = truncate(next.Name, 128)
	s.scimSaveUser(w, r, u, &next)
}

func (s *Server) scimLoadUser(w http.ResponseWriter, r *http.Request) (*api.User, bool) {
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		scimError(w, http.StatusNotFound, "", "user not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return u, true
}

// scimSaveUser applies a SCIM change. Any user the directory touches
// becomes provisioned (managed by the directory).
func (s *Server) scimSaveUser(w http.ResponseWriter, r *http.Request, old, next *api.User) {
	ctx := r.Context()
	if _, err := mail.ParseAddress(next.Email); err != nil {
		scimError(w, http.StatusBadRequest, "invalidValue", "the user's email is not valid")
		return
	}
	now := s.now()
	next.Provisioned, next.UpdatedAt = true, &now
	var err error
	if next.Email != old.Email {
		err = s.store.UpdateUserEmail(ctx, next)
	} else {
		err = s.store.UpdateUser(ctx, next)
	}
	if errors.Is(err, store.ErrConflict) {
		scimError(w, http.StatusConflict, "uniqueness", "another user has this email")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	switch {
	case next.Disabled && !old.Disabled:
		if err := s.store.DeleteUserSessions(ctx, next.ID); err != nil {
			s.internalError(w, err)
			return
		}
		s.logEvent(ctx, "user.updated", "", "", next.Email+" deactivated in the directory (signed out everywhere)")
	case !next.Disabled && old.Disabled:
		s.logEvent(ctx, "user.updated", "", "", next.Email+" reactivated in the directory")
	case !old.Provisioned:
		s.logEvent(ctx, "user.updated", "", "", next.Email+" is now managed by the directory")
	}
	if err := s.refreshGroupAccess(ctx, []string{next.ID}); err != nil {
		s.internalError(w, err)
		return
	}
	next, _ = s.store.GetUser(ctx, next.ID)
	groups, err := s.store.ListSCIMGroups(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	scimJSON(w, http.StatusOK, s.userResource(r, next, groups))
}

func (s *Server) handleSCIMDeleteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, ok := s.scimLoadUser(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteUser(ctx, u.ID); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.deleted", "", "", u.Email+" deleted by the directory")
	w.WriteHeader(http.StatusNoContent)
}

// ---- groups ----

func (s *Server) usersByID(ctx context.Context) (map[string]*api.User, error) {
	us, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]*api.User{}
	for _, u := range us {
		m[u.ID] = u
	}
	return m, nil
}

func (s *Server) handleSCIMListGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attr, value, err := scimFilter(r.URL.Query().Get("filter"))
	if err != nil {
		scimError(w, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	groups, err := s.store.ListSCIMGroups(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	users, err := s.usersByID(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var match []*api.SCIMGroup
	for _, g := range groups {
		switch attr {
		case "":
		case "displayname":
			if !strings.EqualFold(g.DisplayName, value) {
				continue
			}
		case "externalid":
			if g.ExternalID != value {
				continue
			}
		case "id":
			if g.ID != value {
				continue
			}
		default:
			scimError(w, http.StatusBadRequest, "invalidFilter", "cannot filter groups by "+attr)
			return
		}
		match = append(match, g)
	}
	withMembers := !strings.Contains(strings.ToLower(r.URL.Query().Get("excludedAttributes")), "members")
	start, end := scimPage(r, len(match))
	res := []any{}
	for _, g := range match[min(start-1, len(match)):end] {
		res = append(res, s.groupResource(r, g, users, withMembers))
	}
	scimJSON(w, http.StatusOK, scimList(res, start, len(match)))
}

func (s *Server) scimLoadGroup(w http.ResponseWriter, r *http.Request) (*api.SCIMGroup, bool) {
	g, err := s.store.GetSCIMGroup(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		scimError(w, http.StatusNotFound, "", "group not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, err)
		return nil, false
	}
	return g, true
}

func (s *Server) handleSCIMGetGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.scimLoadGroup(w, r)
	if !ok {
		return
	}
	users, err := s.usersByID(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	withMembers := !strings.Contains(strings.ToLower(r.URL.Query().Get("excludedAttributes")), "members")
	scimJSON(w, http.StatusOK, s.groupResource(r, g, users, withMembers))
}

type groupInput struct {
	DisplayName string      `json:"displayName"`
	ExternalID  string      `json:"externalId"`
	Members     []scimMulti `json:"members"`
}

func memberIDs(ms []scimMulti) []string {
	var out []string
	for _, m := range ms {
		if m.Value != "" {
			out = append(out, m.Value)
		}
	}
	return out
}

func (s *Server) handleSCIMCreateGroup(w http.ResponseWriter, r *http.Request) {
	var in groupInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		scimError(w, http.StatusBadRequest, "invalidValue", "displayName is required")
		return
	}
	now := s.now()
	g := &api.SCIMGroup{ID: newID("grp"), DisplayName: truncate(strings.TrimSpace(in.DisplayName), 256), ExternalID: truncate(in.ExternalID, 256),
		Members: uniq(memberIDs(in.Members)), CreatedAt: now, UpdatedAt: now}
	s.scimSaveGroup(w, r, nil, g, http.StatusCreated)
}

func (s *Server) handleSCIMReplaceGroup(w http.ResponseWriter, r *http.Request) {
	old, ok := s.scimLoadGroup(w, r)
	if !ok {
		return
	}
	var in groupInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&in); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	g := *old
	if strings.TrimSpace(in.DisplayName) != "" {
		g.DisplayName = truncate(strings.TrimSpace(in.DisplayName), 256)
	}
	g.ExternalID = truncate(in.ExternalID, 256)
	g.Members = uniq(memberIDs(in.Members))
	s.scimSaveGroup(w, r, old, &g, http.StatusOK)
}

var memberFilterRe = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]+)"\s*\]$`)

func (s *Server) handleSCIMPatchGroup(w http.ResponseWriter, r *http.Request) {
	old, ok := s.scimLoadGroup(w, r)
	if !ok {
		return
	}
	var req patchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil {
		scimError(w, http.StatusBadRequest, "invalidSyntax", err.Error())
		return
	}
	g := *old
	members := map[string]bool{}
	for _, m := range old.Members {
		members[m] = true
	}
	parseMembers := func(raw json.RawMessage) []string {
		var ms []scimMulti
		if json.Unmarshal(raw, &ms) != nil {
			var one scimMulti // some providers send a single object
			if json.Unmarshal(raw, &one) == nil {
				ms = []scimMulti{one}
			}
		}
		return memberIDs(ms)
	}
	for _, op := range req.Operations {
		kind := strings.ToLower(op.Op)
		path := strings.TrimSpace(op.Path)
		lp := strings.ToLower(path)
		switch {
		case path == "" && (kind == "add" || kind == "replace"):
			var in struct {
				DisplayName *string         `json:"displayName"`
				ExternalID  *string         `json:"externalId"`
				Members     json.RawMessage `json:"members"`
			}
			if err := json.Unmarshal(op.Value, &in); err != nil {
				scimError(w, http.StatusBadRequest, "invalidValue", "an operation without a path needs an object value")
				return
			}
			if in.DisplayName != nil && *in.DisplayName != "" {
				g.DisplayName = truncate(*in.DisplayName, 256)
			}
			if in.ExternalID != nil {
				g.ExternalID = *in.ExternalID
			}
			if in.Members != nil {
				if kind == "replace" {
					members = map[string]bool{}
				}
				for _, id := range parseMembers(in.Members) {
					members[id] = true
				}
			}
		case lp == "members" && kind == "add":
			for _, id := range parseMembers(op.Value) {
				members[id] = true
			}
		case lp == "members" && kind == "replace":
			members = map[string]bool{}
			for _, id := range parseMembers(op.Value) {
				members[id] = true
			}
		case lp == "members" && kind == "remove":
			ids := parseMembers(op.Value)
			if len(op.Value) == 0 || string(op.Value) == "null" {
				members = map[string]bool{}
			}
			for _, id := range ids {
				delete(members, id)
			}
		case kind == "remove" && memberFilterRe.MatchString(path):
			delete(members, memberFilterRe.FindStringSubmatch(path)[1])
		case lp == "displayname" && kind != "remove":
			var v string
			if json.Unmarshal(op.Value, &v) == nil && v != "" {
				g.DisplayName = truncate(v, 256)
			}
		case lp == "externalid":
			var v string
			_ = json.Unmarshal(op.Value, &v)
			g.ExternalID = v
		default:
			scimError(w, http.StatusBadRequest, "invalidPath", "unsupported group operation "+op.Op+" "+op.Path)
			return
		}
	}
	g.Members = nil
	for id := range members {
		g.Members = append(g.Members, id)
	}
	sort.Strings(g.Members)
	s.scimSaveGroup(w, r, old, &g, http.StatusOK)
}

func uniq(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) scimSaveGroup(w http.ResponseWriter, r *http.Request, old, g *api.SCIMGroup, code int) {
	ctx := r.Context()
	users, err := s.usersByID(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Members must be existing users.
	var members []string
	for _, id := range g.Members {
		if users[id] != nil {
			members = append(members, id)
		}
	}
	g.Members = members
	g.UpdatedAt = s.now()
	if err := s.store.PutSCIMGroup(ctx, g); errors.Is(err, store.ErrConflict) {
		scimError(w, http.StatusConflict, "uniqueness", "a group with this name already exists")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	affected := append([]string{}, g.Members...)
	if old != nil {
		affected = append(affected, old.Members...)
	}
	if err := s.refreshGroupAccess(ctx, uniq(affected)); err != nil {
		s.internalError(w, err)
		return
	}
	users, _ = s.usersByID(ctx)
	scimJSON(w, code, s.groupResource(r, g, users, true))
}

func (s *Server) handleSCIMDeleteGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g, ok := s.scimLoadGroup(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSCIMGroup(ctx, g.ID); err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.refreshGroupAccess(ctx, g.Members); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- discovery ----

func (s *Server) handleSCIMServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	scimJSON(w, http.StatusOK, map[string]any{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"documentationUri": "https://github.com/nasi-lemak/armada#scim-provisioning",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": 1000},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "Bearer token",
			"description": "A SCIM token created under Users in the Armada dashboard", "primary": true}},
		"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": s.scimBase(r) + "/ServiceProviderConfig"},
	})
}

func (s *Server) handleSCIMResourceTypes(w http.ResponseWriter, r *http.Request) {
	rt := func(name, endpoint, schema string) map[string]any {
		return map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:ResourceType"}, "id": name, "name": name,
			"endpoint": endpoint, "schema": schema, "meta": map[string]any{"resourceType": "ResourceType", "location": s.scimBase(r) + "/ResourceTypes/" + name}}
	}
	scimJSON(w, http.StatusOK, scimList([]any{rt("User", "/Users", scimUserSchema), rt("Group", "/Groups", scimGroupSchema)}, 1, 2))
}

func (s *Server) handleSCIMSchemas(w http.ResponseWriter, r *http.Request) {
	attr := func(name, typ string, required bool) map[string]any {
		return map[string]any{"name": name, "type": typ, "multiValued": false, "required": required, "caseExact": false,
			"mutability": "readWrite", "returned": "default", "uniqueness": "none"}
	}
	user := map[string]any{"id": scimUserSchema, "name": "User", "attributes": []any{attr("userName", "string", true),
		attr("displayName", "string", false), attr("active", "boolean", false), attr("externalId", "string", false)}}
	group := map[string]any{"id": scimGroupSchema, "name": "Group", "attributes": []any{attr("displayName", "string", true)}}
	scimJSON(w, http.StatusOK, scimList([]any{user, group}, 1, 2))
}

// ---- SCIM tokens (admin) ----

func (s *Server) handleListSCIMTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListSCIMTokens(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(ts))
}

func (s *Server) handleCreateSCIMToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	name := truncate(strings.TrimSpace(req.Name), 64)
	if name == "" {
		name = "SCIM"
	}
	secret := newSecret("scim")
	t := &api.SCIMToken{ID: newID("sct"), Name: name, CreatedAt: s.now()}
	if err := s.store.CreateSCIMToken(r.Context(), t, hashSecret(secret)); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "scim.token", "", "", "SCIM token "+name+" created")
	t.Token, t.URL = secret, s.scimBase(r)
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteSCIMToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSCIMToken(r.Context(), r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "scim.token", "", "", "SCIM token revoked")
	w.WriteHeader(http.StatusNoContent)
}
