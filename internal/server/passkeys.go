package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
)

// Passkeys (WebAuthn) are used two ways:
//   - passwordless sign-in: a discoverable passkey with user verification
//     (fingerprint, face, device PIN) is both factors at once;
//   - second factor after a password, like an authenticator code. Security
//     keys without discoverable storage work only this way.
//
// Credentials are bound to the relying-party ID, the host name of the
// server's public URL, so a phishing site can't use them.

const (
	passkeyCeremonyTTL = 5 * time.Minute
	// passkeyReauthWindow is how recent a sign-in must be to add a passkey,
	// so a stolen session can't quietly plant a permanent way in.
	passkeyReauthWindow = 15 * time.Minute
)

type passkeyState struct {
	mu         sync.Mutex
	ceremonies map[string]passkeyCeremony
	byOrigin   map[string]*webauthn.WebAuthn
}

type passkeyCeremony struct {
	kind      string // register | login | 2fa
	userID    string
	challenge string // 2fa: the password step's challenge
	origin    string
	session   webauthn.SessionData
	expires   time.Time
}

// webauthnFor returns the relying party for the server's public origin.
func (s *Server) webauthnFor(r *http.Request) (*webauthn.WebAuthn, string, error) {
	if s.cfg.Android.PublicURL == "" {
		// Passkeys are bound to the server's domain; never take it from
		// request headers.
		return nil, "", errors.New("passkeys need the server's address: start it with -public-url")
	}
	origin := s.publicURL(r)
	ps := &s.passkeys
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if wa := ps.byOrigin[origin]; wa != nil {
		return wa, origin, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return nil, "", errors.New("passkeys need the server's public URL (-public-url)")
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:                  u.Hostname(),
		RPDisplayName:         s.cfg.TOTPIssuer,
		RPOrigins:             []string{origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
	})
	if err != nil {
		return nil, "", err
	}
	if ps.byOrigin == nil {
		ps.byOrigin = map[string]*webauthn.WebAuthn{}
	}
	ps.byOrigin[origin] = wa
	return wa, origin, nil
}

func (s *Server) newCeremony(c passkeyCeremony) (string, error) {
	ps := &s.passkeys
	ps.mu.Lock()
	defer ps.mu.Unlock()
	now := s.now()
	if ps.ceremonies == nil {
		ps.ceremonies = map[string]passkeyCeremony{}
	}
	for k, v := range ps.ceremonies {
		if now.After(v.expires) {
			delete(ps.ceremonies, k)
		}
	}
	if len(ps.ceremonies) > 10000 {
		return "", errors.New("too many passkey sign-ins in progress")
	}
	id := newSecret("pk")
	c.expires = now.Add(passkeyCeremonyTTL)
	ps.ceremonies[id] = c
	return id, nil
}

// takeCeremony returns and removes a ceremony (each is single-use).
func (s *Server) takeCeremony(id, kind, origin string) (passkeyCeremony, bool) {
	ps := &s.passkeys
	ps.mu.Lock()
	defer ps.mu.Unlock()
	c, ok := ps.ceremonies[id]
	delete(ps.ceremonies, id)
	if !ok || c.kind != kind || c.origin != origin || s.now().After(c.expires) {
		return passkeyCeremony{}, false
	}
	return c, true
}

// waUser adapts an Armada user to the WebAuthn library.
type waUser struct {
	u      *api.User
	stored []store.StoredPasskey
	creds  []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte   { return []byte(w.u.ID) }
func (w *waUser) WebAuthnName() string { return w.u.Email }
func (w *waUser) WebAuthnDisplayName() string {
	if w.u.Name != "" {
		return w.u.Name
	}
	return w.u.Email
}
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Server) loadWAUser(ctx context.Context, u *api.User) (*waUser, error) {
	stored, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u, stored: stored}
	for _, p := range stored {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(p.Credential), &c); err != nil {
			return nil, fmt.Errorf("passkey %s: %w", p.ID, err)
		}
		w.creds = append(w.creds, c)
	}
	return w, nil
}

func credID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// recordUse saves the credential's new signature counter, refusing a
// credential whose counter went backwards (a cloned security key).
func (s *Server) recordUse(ctx context.Context, cred *webauthn.Credential) error {
	if cred.Authenticator.CloneWarning {
		return errors.New("this security key's signature counter went backwards; it may have been cloned")
	}
	b, _ := json.Marshal(cred)
	return s.store.UsePasskey(ctx, credID(cred.ID), string(b), s.now())
}

// syncPasskeyCount keeps the user's passkey count (shown in lists and used
// to decide whether sign-in needs a second step) in line with the store.
func (s *Server) syncPasskeyCount(ctx context.Context, u *api.User) error {
	ps, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return err
	}
	u.Passkeys = len(ps)
	return s.store.UpdateUser(ctx, u)
}

func passkeyView(p store.StoredPasskey) api.Passkey {
	v := api.Passkey{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt}
	if !p.LastUsedAt.IsZero() {
		t := p.LastUsedAt
		v.LastUsedAt = &t
	}
	var c webauthn.Credential
	if json.Unmarshal([]byte(p.Credential), &c) == nil {
		v.Synced = c.Flags.BackupEligible
	}
	return v
}

// ---- managing your passkeys ----

func (s *Server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.user == nil {
		writeJSON(w, http.StatusOK, []api.Passkey{})
		return
	}
	ps, err := s.store.ListPasskeys(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := []api.Passkey{}
	for _, x := range ps {
		out = append(out, passkeyView(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	if p.user.NoPassword {
		writeError(w, http.StatusBadRequest, "you sign in with single sign-on; add passkeys at your sign-in provider")
		return
	}
	if s.now().Sub(p.signedInAt) > passkeyReauthWindow {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "sign in again to add a passkey (for your safety this needs a sign-in from the last 15 minutes)", "reauth_required": true})
		return
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.loadWAUser(ctx, p.user)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclude = append(exclude, c.Descriptor())
	}
	opts, session, err := wa.BeginRegistration(user, webauthn.WithExclusions(exclude),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred))
	if err != nil {
		s.internalError(w, err)
		return
	}
	id, err := s.newCeremony(passkeyCeremony{kind: "register", userID: p.user.ID, origin: origin, session: *session})
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.PasskeyCeremony{Ceremony: id, Options: opts})
}

func (s *Server) handlePasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	var req api.PasskeyFinish
	if !readJSON(w, r, &req) {
		return
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, ok := s.takeCeremony(req.Ceremony, "register", origin)
	if !ok || c.userID != p.user.ID {
		writeError(w, http.StatusBadRequest, "this passkey setup expired; try again")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the browser's passkey response is invalid")
		return
	}
	user, err := s.loadWAUser(ctx, p.user)
	if err != nil {
		s.internalError(w, err)
		return
	}
	cred, err := wa.CreateCredential(user, c.session, parsed)
	if err != nil {
		s.log.Info("passkey registration rejected", "user", p.user.Email, "err", err)
		writeError(w, http.StatusBadRequest, "the passkey could not be verified")
		return
	}
	name := truncate(strings.TrimSpace(req.Name), 64)
	if name == "" {
		name = "Passkey"
	}
	b, _ := json.Marshal(cred)
	sp := store.StoredPasskey{ID: credID(cred.ID), UserID: p.user.ID, Name: name, Credential: string(b), CreatedAt: s.now()}
	if err := s.store.AddPasskey(ctx, sp); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "this passkey is already registered")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	u := *p.user
	first := !hasSecondFactor(&u)
	if err := s.syncPasskeyCount(ctx, &u); err != nil {
		s.internalError(w, err)
		return
	}
	if first { // other sessions only had the password
		if err := s.signOutOtherSessions(ctx, &u, p.sessionHash); err != nil {
			s.internalError(w, err)
			return
		}
	}
	s.logEvent(ctx, "user.passkey", "", "", fmt.Sprintf("%s added a passkey (%s)", u.Email, name))
	writeJSON(w, http.StatusCreated, passkeyView(sp))
}

func (s *Server) handleDeletePasskey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	if s.cfg.Require2FA && !p.user.TwoFactor && p.user.Passkeys <= 1 {
		writeError(w, http.StatusForbidden, "this server requires two-factor sign-in: add another passkey or an authenticator app first")
		return
	}
	if err := s.store.DeletePasskey(ctx, p.user.ID, r.PathValue("id")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "passkey not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	u := *p.user
	if err := s.syncPasskeyCount(ctx, &u); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.passkey", "", "", u.Email+" removed a passkey")
	w.WriteHeader(http.StatusNoContent)
}

// ---- passwordless sign-in ----

func (s *Server) handlePasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PasswordLogin {
		writeError(w, http.StatusForbidden, "sign-in with Armada accounts is turned off; use single sign-on")
		return
	}
	if s.logins.blocked([]string{"ip:" + s.clientIP(r)}, s.now()) {
		writeError(w, http.StatusTooManyRequests, "too many failed sign-ins; try again in 15 minutes")
		return
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Discoverable: the browser offers the passkeys it has for this site.
	// User verification is required, because the passkey is both factors.
	opts, session, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.internalError(w, err)
		return
	}
	id, err := s.newCeremony(passkeyCeremony{kind: "login", origin: origin, session: *session})
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.PasskeyCeremony{Ceremony: id, Options: opts})
}

func (s *Server) handlePasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.PasskeyFinish
	if !readJSON(w, r, &req) {
		return
	}
	if !s.cfg.PasswordLogin {
		writeError(w, http.StatusForbidden, "sign-in with Armada accounts is turned off; use single sign-on")
		return
	}
	fail := func(msg string) {
		s.logins.fail([]string{"ip:" + s.clientIP(r)}, s.now())
		writeError(w, http.StatusUnauthorized, msg)
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, ok := s.takeCeremony(req.Ceremony, "login", origin)
	if !ok {
		writeError(w, http.StatusUnauthorized, "this sign-in expired; try again")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		fail("the browser's passkey response is invalid")
		return
	}
	var found *waUser
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		u, err := s.store.GetUser(ctx, string(userHandle))
		if err != nil {
			return nil, errors.New("unknown passkey")
		}
		if found, err = s.loadWAUser(ctx, u); err != nil {
			return nil, err
		}
		return found, nil
	}
	_, cred, err := wa.ValidatePasskeyLogin(handler, c.session, parsed)
	if err != nil || found == nil {
		s.log.Info("passkey sign-in rejected", "err", err)
		fail("this passkey isn't registered here, or couldn't be verified")
		return
	}
	u := found.u
	if u.Disabled || u.NoPassword {
		fail("this account can't sign in with a passkey")
		return
	}
	if err := s.recordUse(ctx, cred); err != nil {
		fail(err.Error())
		return
	}
	s.completeLogin(w, r, u, "passkey", "passkey", "")
}

// ---- passkey as the second step after a password ----

func (s *Server) handlePasskeyTwoFactorBegin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.TwoFactorLoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	userID, ok := s.mfa.take(req.Challenge, s.now())
	if !ok {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil || u.Disabled {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, err := s.loadWAUser(ctx, u)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(user.creds) == 0 {
		writeError(w, http.StatusBadRequest, "you have no passkeys; use your authenticator app")
		return
	}
	opts, session, err := wa.BeginLogin(user)
	if err != nil {
		s.internalError(w, err)
		return
	}
	id, err := s.newCeremony(passkeyCeremony{kind: "2fa", userID: u.ID, challenge: req.Challenge, origin: origin, session: *session})
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.PasskeyCeremony{Ceremony: id, Options: opts})
}

func (s *Server) handlePasskeyTwoFactorFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.PasskeyFinish
	if !readJSON(w, r, &req) {
		return
	}
	wa, origin, err := s.webauthnFor(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, ok := s.takeCeremony(req.Ceremony, "2fa", origin)
	if !ok || c.challenge != req.Challenge {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	userID, ok := s.mfa.take(req.Challenge, s.now()) // counts as an attempt
	if !ok || userID != c.userID {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil || u.Disabled {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Credential))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the browser's passkey response is invalid")
		return
	}
	user, err := s.loadWAUser(ctx, u)
	if err != nil {
		s.internalError(w, err)
		return
	}
	cred, err := wa.ValidateLogin(user, c.session, parsed)
	if err == nil {
		err = s.recordUse(ctx, cred)
	}
	if err != nil {
		s.log.Info("passkey second factor rejected", "user", u.Email, "err", err)
		s.logins.fail(loginKeys(u.Email, s.clientIP(r)), s.now())
		writeError(w, http.StatusUnauthorized, "the passkey couldn't be verified")
		return
	}
	s.completeLogin(w, r, u, "password", "passkey", req.Challenge)
}
