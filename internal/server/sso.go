package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// OIDCConfig configures single sign-on with any OpenID Connect provider:
// Google Workspace, Microsoft Entra ID, Okta, Auth0, Keycloak, Authentik…
type OIDCConfig struct {
	// Issuer is the provider's issuer URL, e.g. https://accounts.google.com.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is <public URL>/api/v1/auth/sso/callback; derived from the
	// request (or -public-url) when empty. Register it with the provider.
	RedirectURL string
	// Name labels the sign-in button, e.g. "Google".
	Name string
	// Scopes requested; default openid, email, profile.
	Scopes []string
	// AllowedDomains, if set, limits sign-in to these email domains.
	AllowedDomains []string
	// CreateRole creates unknown users on first sign-in with this role.
	// Empty means only users an admin added can sign in.
	CreateRole string
	// GroupsClaim names the ID-token claim holding group names (e.g.
	// "groups"). With GroupRoles it sets each user's role at every sign-in.
	GroupsClaim string
	// GroupRoles maps group names to roles; the highest match wins. Users in
	// none of the groups can't sign in.
	GroupRoles map[string]string
}

type ssoState struct {
	cfg OIDCConfig

	mu       sync.Mutex
	provider *oidc.Provider
	pending  map[string]ssoPending // by state
	tickets  map[string]ssoTicket
}

type ssoPending struct {
	nonce, verifier, redirect string
	expires                   time.Time
}

type ssoTicket struct {
	resp    api.LoginResponse
	expires time.Time
}

const (
	ssoCookie     = "armada_sso"
	ssoPendingTTL = 10 * time.Minute
	ssoTicketTTL  = time.Minute
)

func newSSO(cfg OIDCConfig) (*ssoState, error) {
	if cfg.ClientID == "" {
		return nil, errors.New("single sign-on needs a client ID")
	}
	if cfg.CreateRole != "" && !validRole(cfg.CreateRole) {
		return nil, fmt.Errorf("invalid SSO create role %q", cfg.CreateRole)
	}
	for g, r := range cfg.GroupRoles {
		if !validRole(r) {
			return nil, fmt.Errorf("SSO group %q maps to invalid role %q", g, r)
		}
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	if cfg.Name == "" {
		cfg.Name = "single sign-on"
	}
	for i, d := range cfg.AllowedDomains {
		cfg.AllowedDomains[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
	}
	return &ssoState{cfg: cfg, pending: map[string]ssoPending{}, tickets: map[string]ssoTicket{}}, nil
}

// getProvider fetches the provider's discovery document on first use (and
// again after a failure), so the server starts even if the provider is down.
func (st *ssoState) getProvider(ctx context.Context) (*oidc.Provider, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.provider != nil {
		return st.provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, st.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	st.provider = p
	return p, nil
}

func (s *Server) ssoRedirectURL(r *http.Request) string {
	if s.sso.cfg.RedirectURL != "" {
		return s.sso.cfg.RedirectURL
	}
	return s.publicURL(r) + "/api/v1/auth/sso/callback"
}

func (s *Server) oauthConfig(p *oidc.Provider, redirect string) *oauth2.Config {
	return &oauth2.Config{ClientID: s.sso.cfg.ClientID, ClientSecret: s.sso.cfg.ClientSecret,
		Endpoint: p.Endpoint(), RedirectURL: redirect, Scopes: s.sso.cfg.Scopes}
}

// ssoFail sends the browser back to the sign-in page with a message.
func ssoFail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/#/sso?error="+url.QueryEscape(msg), http.StatusFound)
}

// handleSSOLogin starts the authorization-code flow (with PKCE and a nonce).
func (s *Server) handleSSOLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeError(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	p, err := s.sso.getProvider(r.Context())
	if err != nil {
		s.log.Error("SSO provider unavailable", "issuer", s.sso.cfg.Issuer, "err", err)
		ssoFail(w, r, "The sign-in provider can't be reached right now. Try again shortly.")
		return
	}
	now := s.now()
	state, nonce, verifier := newSecret("sso"), newSecret("n"), oauth2.GenerateVerifier()
	redirect := s.ssoRedirectURL(r)
	s.sso.mu.Lock()
	for k, v := range s.sso.pending {
		if now.After(v.expires) {
			delete(s.sso.pending, k)
		}
	}
	if len(s.sso.pending) > 10000 {
		s.sso.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many sign-ins in progress")
		return
	}
	s.sso.pending[state] = ssoPending{nonce: nonce, verifier: verifier, redirect: redirect, expires: now.Add(ssoPendingTTL)}
	s.sso.mu.Unlock()
	// The cookie ties the callback to this browser (login CSRF protection).
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: state, Path: "/api/v1/auth/sso/", MaxAge: int(ssoPendingTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(redirect, "https://"), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, s.oauthConfig(p, redirect).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// handleSSOCallback finishes the flow: verifies the ID token, finds (or
// creates) the user, starts a session and hands the browser a one-time
// ticket in the URL fragment, never the session token itself.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeError(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/api/v1/auth/sso/", MaxAge: -1})
	state := q.Get("state")
	c, err := r.Cookie(ssoCookie)
	if err != nil || state == "" || c.Value != state {
		ssoFail(w, r, "This sign-in link didn't start in this browser. Try again.")
		return
	}
	s.sso.mu.Lock()
	pend, ok := s.sso.pending[state]
	delete(s.sso.pending, state)
	s.sso.mu.Unlock()
	if !ok || s.now().After(pend.expires) {
		ssoFail(w, r, "This sign-in took too long. Try again.")
		return
	}
	if e := q.Get("error"); e != "" {
		msg := q.Get("error_description")
		if msg == "" {
			msg = e
		}
		ssoFail(w, r, "The sign-in provider said: "+truncate(msg, 200))
		return
	}
	p, err := s.sso.getProvider(ctx)
	if err != nil {
		ssoFail(w, r, "The sign-in provider can't be reached right now. Try again shortly.")
		return
	}
	xctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tok, err := s.oauthConfig(p, pend.redirect).Exchange(xctx, q.Get("code"), oauth2.VerifierOption(pend.verifier))
	if err != nil {
		s.log.Warn("SSO code exchange failed", "err", err)
		ssoFail(w, r, "Sign-in failed while talking to the provider. Try again.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := p.Verifier(&oidc.Config{ClientID: s.sso.cfg.ClientID}).Verify(xctx, raw)
	if err != nil || idt.Nonce != pend.nonce {
		s.log.Warn("SSO ID token rejected", "err", err)
		ssoFail(w, r, "The sign-in could not be verified.")
		return
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		ssoFail(w, r, "The sign-in could not be verified.")
		return
	}
	u, msg, err := s.ssoUser(ctx, idt.Issuer, idt.Subject, claims)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if u == nil {
		s.log.Info("SSO sign-in refused", "subject", idt.Subject, "reason", msg)
		ssoFail(w, r, msg)
		return
	}
	u.SSO = true
	stok, exp, err := s.startSession(ctx, u, "sso")
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(context.WithValue(ctx, principalKey{}, &principal{user: u, name: u.Email, role: u.Role}), "user.login", "", "", u.Email+" signed in with "+s.sso.cfg.Name)
	ticket := newSecret("tk")
	s.sso.mu.Lock()
	for k, v := range s.sso.tickets {
		if s.now().After(v.expires) {
			delete(s.sso.tickets, k)
		}
	}
	s.sso.tickets[ticket] = ssoTicket{resp: api.LoginResponse{Token: stok, ExpiresAt: exp, User: *u}, expires: s.now().Add(ssoTicketTTL)}
	s.sso.mu.Unlock()
	http.Redirect(w, r, "/#/sso?ticket="+url.QueryEscape(ticket), http.StatusFound)
}

// handleSSOExchange swaps a one-time ticket for the session token.
func (s *Server) handleSSOExchange(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeError(w, http.StatusNotFound, "single sign-on is not configured")
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	s.sso.mu.Lock()
	t, ok := s.sso.tickets[req.Ticket]
	delete(s.sso.tickets, req.Ticket)
	s.sso.mu.Unlock()
	if !ok || s.now().After(t.expires) {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; sign in again")
		return
	}
	writeJSON(w, http.StatusOK, t.resp)
}

// ssoUser maps a verified identity to an Armada user. It returns nil and a
// message for the person when they may not sign in.
func (s *Server) ssoUser(ctx context.Context, issuer, subject string, claims map[string]any) (*api.User, string, error) {
	cfg := s.sso.cfg
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	if v, ok := claims["email_verified"].(bool); ok && !v {
		return nil, "Your email address isn't verified with the sign-in provider.", nil
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, "The sign-in provider didn't share an email address. Ask your admin to allow the email scope.", nil
	}
	if len(cfg.AllowedDomains) > 0 {
		domain := email[strings.LastIndex(email, "@")+1:]
		allowed := false
		for _, d := range cfg.AllowedDomains {
			allowed = allowed || d == domain
		}
		if !allowed {
			return nil, email + " is not in a domain allowed to sign in here.", nil
		}
	}
	mappings, err := s.groupMappings(ctx)
	if err != nil {
		return nil, "", err
	}
	// With group mappings, access comes from the groups claim; work it out
	// up front so people in no mapped group are refused before anything
	// (such as an account) is created.
	groups := claimStrings(claims[cfg.GroupsClaim])
	if len(mappings) > 0 {
		probe := &api.User{GroupsFromSSO: true, Groups: groups}
		applyGroupAccess(probe, mappings, s.cfg.SCIMDefaultRole)
		if probe.NoGroupAccess {
			return nil, "Your account isn't in a group allowed to use Armada.", nil
		}
	}

	var u *api.User
	id, err := s.store.IdentityUser(ctx, issuer, subject)
	switch {
	case err == nil:
		if u, err = s.store.GetUser(ctx, id); err != nil {
			return nil, "", err
		}
	case errors.Is(err, store.ErrNotFound):
		u, _, err = s.store.UserByEmail(ctx, email)
		switch {
		case err == nil:
			if u.SSO {
				// Linked to another identity already: a second provider account
				// with the same email must not take it over.
				return nil, "This Armada account is linked to a different sign-in identity. An admin can unlink it.", nil
			}
		case errors.Is(err, store.ErrNotFound):
			if cfg.CreateRole == "" && len(mappings) == 0 {
				return nil, "There is no Armada account for " + email + ". Ask an admin to add you.", nil
			}
			name, _ := claims["name"].(string)
			u = &api.User{ID: newID("usr"), Email: email, Name: truncate(name, 128), Role: cfg.CreateRole, NoPassword: true, CreatedAt: s.now()}
			if u.Role == "" {
				u.Role = api.RoleViewer // replaced from groups below
			}
			if err := s.store.CreateUser(ctx, u, ""); err != nil {
				return nil, "", err
			}
			s.logEvent(ctx, "user.created", "", "", "user "+email+" created at first single sign-on ("+u.Role+")")
		default:
			return nil, "", err
		}
		if err := s.store.LinkIdentity(ctx, issuer, subject, u.ID); err != nil {
			return nil, "", err
		}
	default:
		return nil, "", err
	}
	if u.Disabled {
		return nil, "Your Armada account is disabled.", nil
	}
	// Directory-provisioned users get their groups from SCIM; others from
	// the claim at each sign-in.
	if len(mappings) > 0 && !u.Provisioned {
		u.Groups, u.GroupsFromSSO = groups, true
	}
	if why := applyGroupAccess(u, mappings, s.cfg.SCIMDefaultRole); why != "" {
		s.logEvent(ctx, "user.updated", "", "", u.Email+": "+why)
	}
	if u.NoGroupAccess {
		_ = s.store.UpdateUser(ctx, u)
		return nil, "Your account isn't in a group allowed to use Armada.", nil
	}
	return u, "", nil
}

func claimStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
