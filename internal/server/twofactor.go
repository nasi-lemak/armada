package server

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/store"
	"github.com/nasi-lemak/armada/internal/totp"
)

// mfaChallenges are password-verified sign-ins waiting for a second factor.
type mfaChallenges struct {
	mu sync.Mutex
	m  map[string]*mfaChallenge
}

type mfaChallenge struct {
	userID   string
	expires  time.Time
	attempts int
}

const (
	mfaChallengeTTL      = 5 * time.Minute
	mfaChallengeAttempts = 5
)

func (c *mfaChallenges) create(userID string, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*mfaChallenge{}
	}
	for k, v := range c.m { // drop expired ones
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	id := newSecret("mfa")
	c.m[id] = &mfaChallenge{userID: userID, expires: now.Add(mfaChallengeTTL)}
	return id
}

// take returns the challenge's user, counting the attempt; a challenge is
// gone after too many attempts or once it succeeds (done).
func (c *mfaChallenges) take(id string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.m[id]
	if ch == nil || now.After(ch.expires) || ch.attempts >= mfaChallengeAttempts {
		delete(c.m, id)
		return "", false
	}
	ch.attempts++
	return ch.userID, true
}

func (c *mfaChallenges) done(id string) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// checkSecondFactor accepts a current authenticator code or an unused
// recovery code.
func (s *Server) checkSecondFactor(ctx context.Context, u *api.User, code string) (bool, string, error) {
	m, err := s.store.GetMFA(ctx, u.ID)
	if err != nil || m.Secret == "" {
		return false, "", err
	}
	code = strings.TrimSpace(code)
	if len(strings.ReplaceAll(code, " ", "")) == 6 {
		secret, err := s.secrets.open(m.Secret)
		if err != nil {
			return false, "", err
		}
		step, ok := totp.Verify(secret, code, s.now(), m.LastStep)
		if !ok {
			return false, "", nil
		}
		ok, err = s.store.AdvanceMFAStep(ctx, u.ID, step) // refuses a concurrent replay
		return ok, "authenticator", err
	}
	ok, err := s.store.UseRecoveryCode(ctx, u.ID, hashSecret(normalizeRecovery(code)))
	return ok, "recovery code", err
}

func normalizeRecovery(c string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(c), "-", ""), " ", ""))
}

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no 0/o, 1/l/i

func newRecoveryCodes() (codes, hashes []string) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		var sb strings.Builder
		for j, x := range b {
			if j == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[int(x)%len(recoveryAlphabet)])
		}
		codes = append(codes, sb.String())
		hashes = append(hashes, hashSecret(normalizeRecovery(sb.String())))
	}
	return codes, hashes
}

// ---- sign-in, second step ----

func (s *Server) handleTwoFactorLogin(w http.ResponseWriter, r *http.Request) {
	var req api.TwoFactorLoginRequest
	if !readJSON(w, r, &req) {
		return
	}
	now := s.now()
	userID, ok := s.mfa.take(req.Challenge, now)
	if !ok {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	u, err := s.store.GetUser(r.Context(), userID)
	if err != nil || u.Disabled {
		writeError(w, http.StatusUnauthorized, "this sign-in has expired; enter your password again")
		return
	}
	s.finishPasswordLogin(w, r, u, req.Code, req.Challenge)
}

// finishPasswordLogin checks the second factor (if the user has one) and
// starts a session. challenge is empty when the code came with the password.
func (s *Server) finishPasswordLogin(w http.ResponseWriter, r *http.Request, u *api.User, code, challenge string) {
	ctx := r.Context()
	now := s.now()
	keys := []string{"email:" + strings.ToLower(u.Email), "ip:" + clientIP(r)}
	factor := ""
	if hasSecondFactor(u) {
		if code == "" {
			var methods []string
			if u.TwoFactor {
				methods = append(methods, "code")
			}
			if u.Passkeys > 0 {
				methods = append(methods, "passkey")
			}
			writeJSON(w, http.StatusOK, api.LoginResponse{TwoFactorRequired: true, Challenge: s.mfa.create(u.ID, now),
				TwoFactorMethods: methods, User: api.User{Email: u.Email}})
			return
		}
		ok, how, err := s.checkSecondFactor(ctx, u, code)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !ok {
			s.logins.fail(keys, now)
			writeError(w, http.StatusUnauthorized, "that code is not valid")
			return
		}
		factor = how
	}
	s.completeLogin(w, r, u, "password", factor, challenge)
}

// hasSecondFactor reports whether password sign-in needs a second step.
func hasSecondFactor(u *api.User) bool { return u.TwoFactor || u.Passkeys > 0 }

// completeLogin starts a session after every check has passed. method is
// the session's sign-in method; factor describes the second factor used.
func (s *Server) completeLogin(w http.ResponseWriter, r *http.Request, u *api.User, method, factor, challenge string) {
	ctx := r.Context()
	if u.NoGroupAccess {
		writeError(w, http.StatusForbidden, "your directory groups don't include access to Armada")
		return
	}
	if challenge != "" {
		s.mfa.done(challenge)
	}
	s.logins.reset("email:" + strings.ToLower(u.Email))
	tok, exp, err := s.startSession(ctx, u, method)
	if err != nil {
		s.internalError(w, err)
		return
	}
	msg := u.Email + " signed in"
	switch {
	case method == "passkey":
		msg += " with a passkey"
	case factor != "":
		msg += " (password + " + factor + ")"
	}
	s.logEvent(context.WithValue(ctx, principalKey{}, &principal{user: u, name: u.Email, role: u.Role}), "user.login", "", "", msg)
	writeJSON(w, http.StatusOK, api.LoginResponse{Token: tok, ExpiresAt: exp, User: *u})
}

// startSession creates a dashboard session and records the sign-in time.
func (s *Server) startSession(ctx context.Context, u *api.User, method string) (string, time.Time, error) {
	now := s.now()
	tok := newSecret("st")
	exp := now.Add(s.cfg.SessionTTL)
	if err := s.store.CreateSession(ctx, newID("ses"), hashSecret(tok), u.ID, method, now, exp); err != nil {
		return "", time.Time{}, err
	}
	u.LastLoginAt = &now
	_ = s.store.UpdateUser(ctx, u)
	return tok, exp, nil
}

// ---- managing your own second factor ----

// requireSessionUser rejects the admin token and personal API tokens:
// second-factor changes must come from someone signed in to the dashboard.
func requireSessionUser(w http.ResponseWriter, p *principal) bool {
	if p.user == nil {
		writeError(w, http.StatusBadRequest, "the admin token has no account settings")
		return false
	}
	if p.sessionHash == "" {
		writeError(w, http.StatusForbidden, "change two-factor settings from the dashboard, not with an API token")
		return false
	}
	return true
}

func (s *Server) handleTwoFactorSetup(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if !requireSessionUser(w, p) {
		return
	}
	if p.user.NoPassword {
		writeError(w, http.StatusBadRequest, "you sign in with single sign-on; your sign-in provider handles two-factor")
		return
	}
	if p.user.TwoFactor {
		writeError(w, http.StatusConflict, "two-factor sign-in is already on; turn it off first to move to a new app")
		return
	}
	m, err := s.store.GetMFA(r.Context(), p.user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	secret := totp.NewSecret()
	m.Pending = s.secrets.seal(secret)
	if err := s.store.PutMFA(r.Context(), p.user.ID, m); err != nil {
		s.internalError(w, err)
		return
	}
	uri := totp.URI(s.cfg.TOTPIssuer, p.user.Email, secret)
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.TwoFactorSetup{Secret: secret, URI: uri, QRPNG: png})
}

type codeRequest struct {
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (s *Server) handleTwoFactorEnable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	var req codeRequest
	if !readJSON(w, r, &req) {
		return
	}
	m, err := s.store.GetMFA(ctx, p.user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if m.Pending == "" {
		writeError(w, http.StatusBadRequest, "start setup first")
		return
	}
	secret, err := s.secrets.open(m.Pending)
	if err != nil {
		s.internalError(w, err)
		return
	}
	step, ok := totp.Verify(secret, req.Code, s.now(), 0)
	if !ok {
		writeError(w, http.StatusBadRequest, "that code doesn't match; check the time on your phone and try the newest code")
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := s.store.PutMFA(ctx, p.user.ID, store.MFA{Secret: m.Pending, LastStep: step, Recovery: hashes}); err != nil {
		s.internalError(w, err)
		return
	}
	u := *p.user
	u.TwoFactor = true
	if err := s.store.UpdateUser(ctx, &u); err != nil {
		s.internalError(w, err)
		return
	}
	// Other sessions were authenticated with the password alone.
	if err := s.signOutOtherSessions(ctx, &u, p.sessionHash); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.2fa", "", "", u.Email+" turned on two-factor sign-in")
	writeJSON(w, http.StatusOK, api.RecoveryCodes{Codes: codes})
}

func (s *Server) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	if s.cfg.Require2FA && !p.user.NoPassword {
		writeError(w, http.StatusForbidden, "this server requires two-factor sign-in")
		return
	}
	var req codeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if ok, _, err := s.checkSecondFactor(ctx, p.user, req.Code); err != nil {
		s.internalError(w, err)
		return
	} else if !ok {
		writeError(w, http.StatusForbidden, "enter a current code from your app (or a recovery code) to turn this off")
		return
	}
	if err := s.clearTwoFactor(ctx, p.user); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.2fa", "", "", p.user.Email+" turned off two-factor sign-in")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := principalFrom(ctx)
	if !requireSessionUser(w, p) {
		return
	}
	var req codeRequest
	if !readJSON(w, r, &req) {
		return
	}
	if ok, _, err := s.checkSecondFactor(ctx, p.user, req.Code); err != nil {
		s.internalError(w, err)
		return
	} else if !ok {
		writeError(w, http.StatusForbidden, "enter a current code from your app")
		return
	}
	m, err := s.store.GetMFA(ctx, p.user.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	codes, hashes := newRecoveryCodes()
	m.Recovery = hashes
	if err := s.store.PutMFA(ctx, p.user.ID, m); err != nil {
		s.internalError(w, err)
		return
	}
	s.logEvent(ctx, "user.2fa", "", "", p.user.Email+" generated new recovery codes")
	writeJSON(w, http.StatusOK, api.RecoveryCodes{Codes: codes})
}

func (s *Server) clearTwoFactor(ctx context.Context, u *api.User) error {
	if err := s.store.DeleteMFA(ctx, u.ID); err != nil {
		return err
	}
	cp := *u
	cp.TwoFactor = false
	return s.store.UpdateUser(ctx, &cp)
}

// signOutOtherSessions ends every session of a user except keep.
func (s *Server) signOutOtherSessions(ctx context.Context, u *api.User, keep string) error {
	return s.store.DeleteUserSessionsExcept(ctx, u.ID, keep)
}
