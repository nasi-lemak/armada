package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Group mappings turn directory groups (pushed over SCIM, or in the SSO
// groups claim) into access grants: each mapped group a user is in gives
// them its role on its devices (or on the whole fleet). On any device the
// highest grant that covers it applies, so someone can be an operator for
// one client and a viewer for another. Once mappings exist, a
// group-managed user in no mapped group has no access at all.

// groupMappings merges -oidc-group-roles (fleet-wide, read-only) with the
// mappings admins manage; a managed mapping wins for the same group.
func (s *Server) groupMappings(ctx context.Context) ([]*api.GroupMapping, error) {
	ms, err := s.store.ListGroupMappings(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, m := range ms {
		have[strings.ToLower(m.Group)] = true
	}
	for g, r := range s.cfg.OIDC.GroupRoles {
		if !have[strings.ToLower(g)] {
			ms = append(ms, &api.GroupMapping{ID: "flag:" + g, Group: g, Role: r, FromFlag: true})
		}
	}
	sort.Slice(ms, func(i, j int) bool { return strings.ToLower(ms[i].Group) < strings.ToLower(ms[j].Group) })
	return ms, nil
}

func scopeEmpty(sel api.Selector) bool { return sel.Channel == "" && len(sel.Labels) == 0 }

// normalizeGrants merges grants on the same devices (keeping the highest
// role), drops grants a fleet-wide grant already covers with an equal or
// higher role, and sorts fleet-wide first.
func normalizeGrants(gs []api.Grant) []api.Grant {
	var out []api.Grant
	for _, g := range gs {
		merged := false
		for i := range out {
			if reflect.DeepEqual(out[i].Scope.Labels, g.Scope.Labels) && out[i].Scope.Channel == g.Scope.Channel {
				if api.RoleRank(g.Role) > api.RoleRank(out[i].Role) {
					out[i].Role = g.Role
				}
				merged = true
			}
		}
		if !merged {
			out = append(out, g)
		}
	}
	fleet := ""
	for _, g := range out {
		if scopeEmpty(g.Scope) && api.RoleRank(g.Role) > api.RoleRank(fleet) {
			fleet = g.Role
		}
	}
	kept := out[:0]
	for _, g := range out {
		if scopeEmpty(g.Scope) || api.RoleRank(g.Role) > api.RoleRank(fleet) {
			kept = append(kept, g)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if scopeEmpty(kept[i].Scope) != scopeEmpty(kept[j].Scope) {
			return scopeEmpty(kept[i].Scope)
		}
		return selText(kept[i].Scope) < selText(kept[j].Scope)
	})
	return kept
}

func highestRole(gs []api.Grant) string {
	r := ""
	for _, g := range gs {
		if api.RoleRank(g.Role) > api.RoleRank(r) {
			r = g.Role
		}
	}
	return r
}

func grantsText(gs []api.Grant) string {
	var parts []string
	for _, g := range gs {
		parts = append(parts, g.Role+" on "+selText(g.Scope))
	}
	return strings.Join(parts, ", ")
}

// applyGroupAccess sets a user's grants and access from their groups. It
// reports a short description of what changed ("" if nothing).
func applyGroupAccess(u *api.User, mappings []*api.GroupMapping, defaultRole string) string {
	state := func() string { return fmt.Sprint(u.Role, u.Grants, u.NoGroupAccess, u.GroupManaged) }
	before := state()
	managed := len(mappings) > 0 && (u.Provisioned || u.GroupsFromSSO)
	if !managed {
		if u.GroupManaged {
			// The mappings that gave this user access are gone. Falling back
			// to their manual settings could widen access (a client account
			// would see the whole fleet), so they get none until an admin
			// sets their access by hand.
			u.GroupManaged, u.Grants, u.NoGroupAccess = false, nil, true
			return "group mappings no longer apply; no access until an admin sets it by hand"
		}
		return ""
	}
	u.GroupManaged = true
	in := map[string]bool{}
	for _, g := range u.Groups {
		in[strings.ToLower(g)] = true
	}
	var grants []api.Grant
	for _, m := range mappings {
		if in[strings.ToLower(m.Group)] {
			grants = append(grants, api.Grant{Role: m.Role, Scope: m.Scope})
		}
	}
	why := "no access (in no mapped group)"
	if len(grants) == 0 {
		u.NoGroupAccess, u.Grants = true, nil
	} else {
		u.Grants = normalizeGrants(grants)
		u.Role, u.NoGroupAccess = highestRole(u.Grants), false
		why = grantsText(u.Grants)
	}
	if state() == before {
		return ""
	}
	return why
}

func scopesText(scopes []api.Selector) string {
	var parts []string
	for _, sc := range scopes {
		parts = append(parts, selText(sc))
	}
	return strings.Join(parts, " or ")
}

// selText renders a selector as "customer=acme site=sg" (sorted).
func selText(sel api.Selector) string {
	var parts []string
	for k, v := range sel.Labels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	if sel.Channel != "" {
		parts = append([]string{"channel=" + sel.Channel}, parts...)
	}
	if len(parts) == 0 {
		return "the whole fleet"
	}
	return strings.Join(parts, " ")
}

// userGrants is the access a user has: their grants, or else their
// single role and scope.
func userGrants(u *api.User) []api.Grant {
	if u.GroupManaged || len(u.Grants) > 0 {
		return u.Grants
	}
	return []api.Grant{{Role: u.Role, Scope: u.Scope}}
}

// refreshGroupAccess recomputes and saves the access of the given users
// (all users when ids is nil), logging each change.
func (s *Server) refreshGroupAccess(ctx context.Context, ids []string) error {
	mappings, err := s.groupMappings(ctx)
	if err != nil {
		return err
	}
	var users []*api.User
	if ids == nil {
		if users, err = s.store.ListUsers(ctx); err != nil {
			return err
		}
	} else {
		for _, id := range ids {
			u, err := s.store.GetUser(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			} else if err != nil {
				return err
			}
			users = append(users, u)
		}
	}
	scimGroups, err := s.store.ListSCIMGroups(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Provisioned { // the directory's groups are authoritative
			u.Groups = nil
			for _, g := range scimGroups {
				for _, m := range g.Members {
					if m == u.ID {
						u.Groups = append(u.Groups, g.DisplayName)
					}
				}
			}
			sort.Strings(u.Groups)
		}
		if why := applyGroupAccess(u, mappings, s.cfg.SCIMDefaultRole); why != "" {
			s.logEvent(ctx, "user.updated", "", "", u.Email+": "+why)
		}
		if err := s.store.UpdateUser(ctx, u); err != nil {
			return err
		}
	}
	return nil
}

// ---- admin API ----

func (s *Server) handleListGroupMappings(w http.ResponseWriter, r *http.Request) {
	ms, err := s.groupMappings(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(ms))
}

func (s *Server) handlePutGroupMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var m api.GroupMapping
	if !readJSON(w, r, &m) {
		return
	}
	m.Group = strings.TrimSpace(m.Group)
	if m.Group == "" || len(m.Group) > 256 {
		writeError(w, http.StatusBadRequest, "group name is required")
		return
	}
	if err := checkUserFields(m.Role, m.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if id := r.PathValue("id"); id != "" {
		m.ID = id
	} else {
		m.ID = newID("gm")
	}
	m.CreatedAt, m.FromFlag = s.now(), false
	if err := s.store.PutGroupMapping(ctx, &m); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "that group is already mapped")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "group.mapping", "", "", fmt.Sprintf("group %s mapped to %s for %s", m.Group, m.Role, selText(m.Scope)))
	if err := s.refreshGroupAccess(ctx, nil); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDeleteGroupMapping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if strings.HasPrefix(r.PathValue("id"), "flag:") {
		writeError(w, http.StatusBadRequest, "this mapping comes from -oidc-group-roles; change the server flag")
		return
	}
	if err := s.store.DeleteGroupMapping(ctx, r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "mapping not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "group.mapping", "", "", "group mapping removed")
	if err := s.refreshGroupAccess(ctx, nil); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
