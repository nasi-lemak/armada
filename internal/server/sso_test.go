package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
)

// fakeIdP is a minimal OpenID Connect provider: discovery, JWKS, an
// authorize endpoint that signs the configured person in at once, and a
// token endpoint that checks PKCE and returns an RS256 ID token.
type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any // who signs in next
	codes  map[string]fakeCode
}

type fakeCode struct{ nonce, challenge string }

func newFakeIdP(t *testing.T) *fakeIdP {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeIdP{key: key, codes: map[string]fakeCode{}}
	b64 := base64.RawURLEncoding.EncodeToString
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE required", 400)
			return
		}
		code := "c" + b64([]byte(time.Now().String()))
		f.mu.Lock()
		f.codes[code] = fakeCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		c, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		claims := map[string]any{}
		for k, v := range f.claims {
			claims[k] = v
		}
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || b64(sum[:]) != c.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, 400)
			return
		}
		claims["iss"], claims["aud"], claims["nonce"] = f.srv.URL, "armada-test", c.nonce
		claims["iat"], claims["exp"] = time.Now().Unix(), time.Now().Add(5*time.Minute).Unix()
		hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		body, _ := json.Marshal(claims)
		signing := b64(hdr) + "." + b64(body)
		h := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": signing + "." + b64(sig)})
	})
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) as(claims map[string]any) {
	f.mu.Lock()
	f.claims = claims
	f.mu.Unlock()
}

// ssoSignIn runs the browser flow and returns the ticket or error message
// from the final redirect.
func ssoSignIn(t *testing.T, ts *httptest.Server, withCookies bool) (ticket, errMsg string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.Fragment, "/sso") {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	if withCookies {
		client.Jar = jar
	}
	resp, err := client.Get(ts.URL + "/api/v1/auth/sso/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q, _ := url.ParseQuery(strings.TrimPrefix(loc.Fragment, "/sso?"))
	return q.Get("ticket"), q.Get("error")
}

func TestSingleSignOn(t *testing.T) {
	idp := newFakeIdP(t)
	s, ts := setupWith(t, func(c *Config) {
		c.OIDC = OIDCConfig{Issuer: idp.srv.URL, ClientID: "armada-test", ClientSecret: "s", Name: "Acme SSO", AllowedDomains: []string{"vendor.com"}}
		c.Require2FA = true // SSO sign-ins are exempt
	})
	s.cfg.Android.PublicURL = ts.URL
	var ac api.AuthConfig
	call(t, ts, "", "GET", "/api/v1/auth/config", nil, &ac)
	if !ac.SSO || ac.SSOName != "Acme SSO" {
		t.Fatalf("auth config: %+v", ac)
	}

	idp.as(map[string]any{"sub": "u-1", "email": "dana@vendor.com", "email_verified": true, "name": "Dana"})
	if _, msg := ssoSignIn(t, ts, true); !strings.Contains(msg, "no Armada account") {
		t.Fatalf("unknown user: %q", msg)
	}
	// An admin adds Dana with no password: SSO only.
	var dana api.User
	if c := call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "dana@vendor.com", Role: api.RoleOperator}, &dana); c != 201 || !dana.NoPassword {
		t.Fatalf("create SSO-only user: %d %+v", c, dana)
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "dana@vendor.com", Password: ""}, nil); c != 401 {
		t.Fatalf("SSO-only user signed in with empty password: %d", c)
	}
	ticket, msg := ssoSignIn(t, ts, true)
	if ticket == "" {
		t.Fatalf("sign-in failed: %s", msg)
	}
	var lr api.LoginResponse
	if c := call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, &lr); c != 200 || lr.Token == "" {
		t.Fatalf("exchange: %d", c)
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, nil); c != 401 {
		t.Fatalf("ticket reused: %d", c)
	}
	var me api.Me
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.SignInMethod != "sso" || me.TwoFactorSetupRequired || !me.User.SSO || me.Role != api.RoleOperator {
		t.Fatalf("me: %+v", me)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, nil); c != 200 {
		t.Fatalf("SSO session blocked by the 2FA requirement: %d", c)
	}

	// Later sign-ins match by subject, even after the email changes.
	idp.as(map[string]any{"sub": "u-1", "email": "dana.lee@vendor.com", "email_verified": true})
	if ticket, msg := ssoSignIn(t, ts, true); ticket == "" {
		t.Fatalf("by subject: %s", msg)
	}
	// Another identity with Dana's old email can't take the account over.
	idp.as(map[string]any{"sub": "attacker", "email": "dana@vendor.com", "email_verified": true})
	if _, msg := ssoSignIn(t, ts, true); !strings.Contains(msg, "different sign-in identity") {
		t.Fatalf("takeover: %q", msg)
	}
	for name, claims := range map[string]map[string]any{
		"other domain": {"sub": "u-2", "email": "eve@gmail.com", "email_verified": true},
		"unverified":   {"sub": "u-3", "email": "sam@vendor.com", "email_verified": false},
		"no email":     {"sub": "u-4"},
	} {
		idp.as(claims)
		if ticket, _ := ssoSignIn(t, ts, true); ticket != "" {
			t.Errorf("%s signed in", name)
		}
	}
	// The callback must come back to the browser that started it.
	idp.as(map[string]any{"sub": "u-1", "email": "dana@vendor.com", "email_verified": true})
	if _, msg := ssoSignIn(t, ts, false); !strings.Contains(msg, "didn't start in this browser") {
		t.Fatalf("missing state cookie: %q", msg)
	}
	// Disabled users are refused.
	call(t, ts, admin, "PATCH", "/api/v1/users/"+dana.ID, map[string]any{"disabled": true}, nil)
	if _, msg := ssoSignIn(t, ts, true); !strings.Contains(msg, "disabled") {
		t.Fatalf("disabled: %q", msg)
	}
	_ = s
}

func TestSingleSignOnGroupsAndAutoCreate(t *testing.T) {
	idp := newFakeIdP(t)
	s, ts := setupWith(t, func(c *Config) {
		c.OIDC = OIDCConfig{Issuer: idp.srv.URL, ClientID: "armada-test", GroupsClaim: "groups",
			GroupRoles: map[string]string{"fleet-admins": api.RoleAdmin, "fleet-ops": api.RoleOperator}}
	})
	s.cfg.Android.PublicURL = ts.URL
	idp.as(map[string]any{"sub": "a", "email": "new@vendor.com", "groups": []string{"staff", "fleet-ops"}})
	ticket, msg := ssoSignIn(t, ts, true)
	if ticket == "" {
		t.Fatalf("group sign-in: %s", msg)
	}
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, &lr)
	if lr.User.Role != api.RoleOperator || !lr.User.NoPassword {
		t.Fatalf("created user: %+v", lr.User)
	}
	// Promoted in the identity provider: the role follows at next sign-in.
	idp.as(map[string]any{"sub": "a", "email": "new@vendor.com", "groups": []string{"fleet-admins", "fleet-ops"}})
	ticket, _ = ssoSignIn(t, ts, true)
	call(t, ts, "", "POST", "/api/v1/auth/sso/exchange", map[string]string{"ticket": ticket}, &lr)
	if lr.User.Role != api.RoleAdmin {
		t.Fatalf("role not updated from groups: %s", lr.User.Role)
	}
	idp.as(map[string]any{"sub": "b", "email": "x@vendor.com", "groups": []string{"staff"}})
	if _, msg := ssoSignIn(t, ts, true); !strings.Contains(msg, "group allowed") {
		t.Fatalf("no matching group: %q", msg)
	}
}
