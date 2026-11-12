package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
)

// principal is whoever is calling the admin API.
type principal struct {
	user *api.User // nil for the server's admin token
	name string    // recorded as the actor of events
	role string    // the highest role in any grant
	// grants are roles on sets of devices (empty scope: the whole fleet).
	grants []api.Grant
	// sessionHash is set when signed in with a dashboard session.
	sessionHash string
	// method is how they signed in: password, passkey, sso, token or admin-token.
	method string
	// signedInAt is when the dashboard session started.
	signedInAt time.Time
	// setupRequired means two-factor sign-in is required and not set up yet:
	// only account and sign-out routes are allowed.
	setupRequired bool
}

// fleetRole is the principal's role on the whole fleet ("" if they only
// have grants on some devices). A nil principal (no auth context) counts as
// fleet-wide admin; every route that reaches these runs behind guard,
// which always sets one.
func (p *principal) fleetRole() string {
	if p == nil {
		return api.RoleAdmin
	}
	r := ""
	for _, g := range p.grants {
		if scopeEmpty(g.Scope) && api.RoleRank(g.Role) > api.RoleRank(r) {
			r = g.Role
		}
	}
	return r
}

// scoped means the principal has no fleet-wide grant: fleet-wide data and
// settings are hidden from them.
func (p *principal) scoped() bool { return p.fleetRole() == "" }

// roleFor is the principal's role on a device: the highest grant covering
// it, or "" if none does.
func (p *principal) roleFor(d *api.Device) string {
	if p == nil {
		return api.RoleAdmin
	}
	r := ""
	for _, g := range p.grants {
		if (scopeEmpty(g.Scope) || rollout.Matches(g.Scope, d)) && api.RoleRank(g.Role) > api.RoleRank(r) {
			r = g.Role
		}
	}
	return r
}

// sees reports whether any grant covers a device.
func (p *principal) sees(d *api.Device) bool { return p.roleFor(d) != "" }

// scopes lists the device scopes of a principal without fleet-wide access.
func (p *principal) scopes() []api.Selector {
	if !p.scoped() {
		return nil
	}
	var out []api.Selector
	for _, g := range p.grants {
		out = append(out, g.Scope)
	}
	return out
}

// needDeviceRole checks the principal's role on one device.
func needDeviceRole(w http.ResponseWriter, r *http.Request, d *api.Device, role string) bool {
	if got := principalFrom(r.Context()).roleFor(d); api.RoleRank(got) < api.RoleRank(role) {
		writeError(w, http.StatusForbidden, "your role on this device ("+got+") cannot do this; it needs "+role)
		return false
	}
	return true
}

type principalKey struct{}

func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey{}).(*principal)
	return p
}

// authenticate resolves the bearer token: the server's admin token, a
// dashboard session (st_…) or a personal API token (ut_…).
func (s *Server) authenticate(r *http.Request) (*principal, error) {
	tok := bearer(r)
	if tok == "" {
		return nil, errors.New("sign in or pass a token")
	}
	for _, t := range s.cfg.AdminTokens {
		if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
			s.noteAdminTokenUse(r)
			return &principal{name: "admin token", role: api.RoleAdmin, method: "admin-token", grants: []api.Grant{{Role: api.RoleAdmin}}}, nil
		}
	}
	var u *api.User
	var err error
	p := &principal{}
	switch {
	case strings.HasPrefix(tok, "st_"):
		p.sessionHash = hashSecret(tok)
		var info store.SessionInfo
		u, info, err = s.store.SessionUser(r.Context(), p.sessionHash, s.now(), s.cfg.SessionTTL)
		p.method, p.signedInAt = info.Method, info.CreatedAt
	case strings.HasPrefix(tok, "ut_"):
		p.method = "token"
		u, err = s.store.TokenUser(r.Context(), hashSecret(tok), s.now())
	default:
		return nil, errors.New("invalid token")
	}
	if err != nil {
		return nil, errors.New("invalid or expired token")
	}
	if u.Disabled {
		return nil, errors.New("this account is disabled")
	}
	if u.NoGroupAccess {
		return nil, errors.New("your directory groups don't include access to Armada")
	}
	p.user, p.name, p.grants = u, u.Email, userGrants(u)
	p.role = highestRole(p.grants)
	p.setupRequired = s.cfg.Require2FA && p.method == "password" && !hasSecondFactor(u)
	return p, nil
}

// allowedDuringSetup are the routes a user who must still set up two-factor
// sign-in can reach.
func allowedDuringSetup(path string) bool {
	return path == "/api/v1/me" || path == "/api/v1/auth/logout" || strings.HasPrefix(path, "/api/v1/me/2fa/") ||
		strings.HasPrefix(path, "/api/v1/me/passkeys")
}

// guard requires at least minRole. Routes that are not scopedOK are refused
// to users limited to some devices (fleet-wide data and settings).
func (s *Server) guard(minRole string, scopedOK bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if p.setupRequired && !allowedDuringSetup(r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "set up two-factor sign-in first (My account)", "two_factor_setup_required": true})
			return
		}
		have := p.role
		if !scopedOK {
			// Fleet-wide data and settings need a fleet-wide grant.
			if have = p.fleetRole(); have == "" {
				writeError(w, http.StatusForbidden, "not available to accounts limited to some devices")
				return
			}
		}
		if api.RoleRank(have) < api.RoleRank(minRole) {
			writeError(w, http.StatusForbidden, "your role ("+have+") cannot do this; it needs "+minRole)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// ---- login ----

// loginLimiter slows password guessing. Within 15 minutes, 5 failures for
// one email lock that email, and 20 failures from one address lock the
// address. The address limit is higher because users behind the same NAT or
// reverse proxy share it.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

const loginWindow = 15 * time.Minute

func loginLimit(key string) int {
	switch {
	case strings.HasPrefix(key, "pair:"):
		return 5
	case strings.HasPrefix(key, "ip:"):
		return 20
	}
	return 50
}

func (l *loginLimiter) recent(k string, now time.Time) int {
	n := 0
	for _, t := range l.failures[k] {
		if now.Sub(t) < loginWindow {
			n++
		}
	}
	return n
}

func (l *loginLimiter) blocked(keys []string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if l.recent(k, now) >= loginLimit(k) {
			return true
		}
	}
	return false
}

func (l *loginLimiter) fail(keys []string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	if len(l.failures) > 10_000 {
		// Drop expired entries only, so flooding can't wipe active lockouts.
		for k, ts := range l.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) >= loginWindow {
				delete(l.failures, k)
			}
		}
	}
	for _, k := range keys {
		if len(l.failures) > 200_000 && l.failures[k] == nil {
			continue // bound memory under a spray of distinct emails
		}
		ts := append(l.failures[k], now)
		if len(ts) > 64 {
			ts = ts[len(ts)-64:]
		}
		l.failures[k] = ts
	}
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.failures, key)
	l.mu.Unlock()
}

// dummyHash keeps login timing the same for unknown emails.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("armada-timing-equaliser"), bcrypt.DefaultCost)

// loginKeys are the throttling buckets for a sign-in attempt: this email
// from this address (tight), this address (looser, shared behind NAT), and
// this email from anywhere (loosest: stops distributed guessing without
// letting one stranger lock a known user out).
func loginKeys(email, ip string) []string {
	email = strings.ToLower(email)
	return []string{"pair:" + email + "|" + ip, "ip:" + ip, "email:" + email}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req api.LoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !s.cfg.PasswordLogin {
		writeError(w, http.StatusForbidden, "password sign-in is turned off; use single sign-on")
		return
	}
	now := s.now()
	email := strings.ToLower(strings.TrimSpace(req.Email))
	keys := loginKeys(email, s.clientIP(r))
	if s.logins.blocked(keys, now) {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins; try again in 15 minutes")
		return
	}
	u, hash, err := s.store.UserByEmail(r.Context(), email)
	if err != nil || hash == "" { // unknown, or an SSO-only account
		hash = string(dummyHash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil || err != nil || u.Disabled || u.NoPassword {
		s.logins.fail(keys, now)
		writeError(w, http.StatusUnauthorized, "wrong email or password")
		return
	}
	s.finishPasswordLogin(w, r, u, req.Code, "")
}

func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	c := api.AuthConfig{PasswordLogin: s.cfg.PasswordLogin, Passkeys: s.cfg.PasswordLogin && s.cfg.Android.PublicURL != ""}
	if s.sso != nil {
		c.SSO, c.SSOName = true, s.cfg.OIDC.Name
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if p := principalFrom(r.Context()); p != nil && p.sessionHash != "" {
		_ = s.store.DeleteSession(r.Context(), p.sessionHash)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	me := api.Me{User: p.user, Name: p.name, Role: p.role, Scopes: p.scopes(), Scoped: p.scoped(),
		Grants: p.grants, FleetRole: p.fleetRole(), SignInMethod: p.method, TwoFactorSetupRequired: p.setupRequired}
	if sc := p.scopes(); len(sc) > 0 {
		me.Scope = sc[0]
	}
	writeJSON(w, http.StatusOK, me)
}

// ---- users (admin) ----

const minPasswordLen = 10

func validRole(r string) bool {
	return r == api.RoleViewer || r == api.RoleOperator || r == api.RoleAdmin
}

func hashPassword(pw string) (string, error) {
	if len(pw) < minPasswordLen {
		return "", errors.New("password must be at least 10 characters")
	}
	if len(pw) > 72 {
		return "", errors.New("password must be at most 72 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

func checkUserFields(role string, scope api.Selector) error {
	if !validRole(role) {
		return errors.New("role must be viewer, operator or admin")
	}
	if err := validLabels(scope.Labels); err != nil {
		return err
	}
	if len(scope.DeviceIDs) > 0 {
		// Scopes follow labels so new devices fall in automatically; a
		// device-ID-only scope would otherwise read as "whole fleet".
		return errors.New("scope users by labels or channel, not device IDs")
	}
	if scope.Channel != "" && !channelRe.MatchString(scope.Channel) {
		return errors.New("invalid scope channel")
	}
	if role == api.RoleAdmin && (scope.Channel != "" || len(scope.Labels) > 0) {
		return errors.New("admins manage the whole fleet; limit operators or viewers to a scope instead")
	}
	return nil
}

// checkGrants validates per-client access and returns it normalised.
func checkGrants(gs []api.Grant) ([]api.Grant, error) {
	if len(gs) == 0 {
		return nil, errors.New("give at least one grant")
	}
	if len(gs) > 100 {
		return nil, errors.New("too many grants")
	}
	for _, g := range gs {
		if err := checkUserFields(g.Role, g.Scope); err != nil {
			return nil, err
		}
	}
	return normalizeGrants(gs), nil
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(us))
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req api.CreateUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	var grants []api.Grant
	if len(req.Grants) > 0 {
		var err error
		if grants, err = checkGrants(req.Grants); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Role, req.Scope = highestRole(grants), api.Selector{}
	} else if err := checkUserFields(req.Role, req.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u := &api.User{ID: newID("usr"), Email: req.Email, Name: truncate(strings.TrimSpace(req.Name), 128), Role: req.Role, Scope: req.Scope, Grants: grants, CreatedAt: s.now()}
	hash := ""
	if req.Password == "" && s.sso != nil {
		u.NoPassword = true // signs in with single sign-on only
	} else {
		var err error
		if hash, err = hashPassword(req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.store.CreateUser(r.Context(), u, hash); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a user with this email already exists")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "user.created", "", "", "user "+u.Email+" created ("+u.Role+scopeNote(u.Scope)+")")
	writeJSON(w, http.StatusCreated, u)
}

func scopeNote(sel api.Selector) string {
	var parts []string
	if sel.Channel != "" {
		parts = append(parts, "channel="+sel.Channel)
	}
	for k, v := range sel.Labels {
		parts = append(parts, k+"="+v)
	}
	if len(parts) == 0 {
		return ""
	}
	return ", limited to " + strings.Join(parts, ",")
}

type patchUserRequest struct {
	Name     *string       `json:"name"`
	Role     *string       `json:"role"`
	Scope    *api.Selector `json:"scope"`
	Disabled *bool         `json:"disabled"`
	Password *string       `json:"password"`
	// Grants replaces the user's access with per-client roles; an empty
	// list goes back to a single Role and Scope.
	Grants *[]api.Grant `json:"grants"`
	// ResetTwoFactor turns off the user's authenticator (lost phone).
	ResetTwoFactor bool `json:"reset_two_factor"`
	// UnlinkSSO forgets the user's single sign-on identity.
	UnlinkSSO bool `json:"unlink_sso"`
}

func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := s.store.GetUser(ctx, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	var req patchUserRequest
	if !readJSON(w, r, &req) {
		return
	}
	me := principalFrom(ctx)
	self := me.user != nil && me.user.ID == u.ID
	if self && ((req.Role != nil && *req.Role != u.Role) || req.Scope != nil || req.Grants != nil || (req.Disabled != nil && *req.Disabled)) {
		writeError(w, http.StatusBadRequest, "you cannot change your own access or disable yourself")
		return
	}
	if req.Name != nil {
		u.Name = truncate(strings.TrimSpace(*req.Name), 128)
	}
	if u.GroupManaged && ((req.Role != nil && *req.Role != u.Role) || req.Scope != nil || req.Grants != nil) {
		writeError(w, http.StatusBadRequest, "this user's role and devices come from their directory groups; change their groups or the group mappings instead")
		return
	}
	if len(u.Grants) > 0 && req.Grants == nil && ((req.Role != nil && *req.Role != u.Role) || req.Scope != nil) {
		writeError(w, http.StatusBadRequest, "this user has per-client access; edit their grants instead")
		return
	}
	if req.Role != nil {
		u.Role = *req.Role
	}
	if !u.GroupManaged && (req.Role != nil || req.Scope != nil || req.Grants != nil) {
		u.NoGroupAccess = false // access now set by hand
	}
	if req.Scope != nil {
		u.Scope = *req.Scope
	}
	if req.Grants != nil {
		if len(*req.Grants) == 0 {
			u.Grants = nil // back to a single role and scope
		} else {
			gs, err := checkGrants(*req.Grants)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			u.Grants, u.Role, u.Scope = gs, highestRole(gs), api.Selector{}
		}
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	if err := checkUserFields(u.Role, u.Scope); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Password != nil {
		hash, err := hashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.store.SetPassword(ctx, u.ID, hash); err != nil {
			s.internalError(w, err)
			return
		}
		u.NoPassword = false
	}
	if req.ResetTwoFactor && hasSecondFactor(u) {
		// Removes every second factor: authenticator, recovery codes, passkeys.
		if err := s.store.DeleteMFA(ctx, u.ID); err != nil {
			s.internalError(w, err)
			return
		}
		if err := s.store.DeletePasskeys(ctx, u.ID); err != nil {
			s.internalError(w, err)
			return
		}
		u.Passkeys = 0
		if err := s.store.DeleteUserSessions(ctx, u.ID); err != nil {
			s.internalError(w, err)
			return
		}
		u.TwoFactor = false
		s.logEvent(ctx, "user.2fa", "", "", "two-factor sign-in reset for "+u.Email+" (signed out everywhere)")
	}
	if req.UnlinkSSO && u.SSO {
		if err := s.store.UnlinkIdentities(ctx, u.ID); err != nil {
			s.internalError(w, err)
			return
		}
		u.SSO = false
	}
	if err := s.store.UpdateUser(ctx, u); err != nil {
		s.internalError(w, err)
		return
	}
	// Disabling takes effect immediately: every request re-checks the account.
	access := u.Role + scopeNote(u.Scope)
	if len(u.Grants) > 0 {
		access = grantsText(u.Grants)
	}
	s.logEvent(ctx, "user.updated", "", "", "user "+u.Email+" updated ("+access+map[bool]string{true: ", disabled", false: ""}[u.Disabled]+")")
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if me := principalFrom(ctx); me.user != nil && me.user.ID == id {
		writeError(w, http.StatusBadRequest, "you cannot delete yourself")
		return
	}
	u, err := s.store.GetUser(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.store.DeleteUser(ctx, id); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.deleted", "", "", "user "+u.Email+" deleted")
	w.WriteHeader(http.StatusNoContent)
}

// ---- my account ----

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeError(w, http.StatusBadRequest, "the admin token has no password")
		return
	}
	if p.user.NoPassword {
		writeError(w, http.StatusBadRequest, "you sign in with single sign-on; there is no password to change")
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	_, hash, err := s.store.UserByEmail(r.Context(), p.user.Email)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Current)) != nil {
		writeError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	nh, err := hashPassword(req.New)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.SetPassword(r.Context(), p.user.ID, nh); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(r.Context(), "user.password", "", "", p.user.Email+" changed their password (all sessions signed out)")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListMyTokens(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeJSON(w, http.StatusOK, []*api.APIToken{})
		return
	}
	ts, err := s.store.ListUserTokens(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

func (s *Server) handleCreateMyToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeError(w, http.StatusBadRequest, "sign in as a user to create personal tokens")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	t := &api.APIToken{ID: newID("utk"), Name: truncate(strings.TrimSpace(req.Name), 64), CreatedAt: s.now(), Token: newSecret("ut")}
	if t.Name == "" {
		t.Name = "token"
	}
	if err := s.store.CreateUserToken(r.Context(), p.user.ID, t, hashSecret(t.Token)); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteMyToken(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	if err := s.store.DeleteUserToken(r.Context(), p.user.ID, r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// noteAdminTokenUse records admin-token use in the activity log, at most
// once per address an hour, so its use is visible without flooding.
func (s *Server) noteAdminTokenUse(r *http.Request) {
	ip := s.clientIP(r)
	now := s.now()
	s.adminSeen.Lock()
	if s.adminSeen.m == nil {
		s.adminSeen.m = map[string]time.Time{}
	}
	last, seen := s.adminSeen.m[ip]
	if !seen || now.Sub(last) > time.Hour {
		s.adminSeen.m[ip] = now
	}
	s.adminSeen.Unlock()
	if !seen || now.Sub(last) > time.Hour {
		ctx := context.WithValue(r.Context(), principalKey{}, &principal{name: "admin token"})
		s.logEvent(ctx, "auth.admin_token", "", "", "admin token used from "+ip)
	}
}
