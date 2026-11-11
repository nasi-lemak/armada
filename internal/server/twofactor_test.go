package server

import (
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/totp"
)

func TestTwoFactorSignIn(t *testing.T) {
	s, ts := setup(t)
	clock := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	tick := func() { clock = clock.Add(30 * time.Second) } // next TOTP step
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "dana@vendor.com", Role: api.RoleAdmin, Password: "correct horse battery"}, nil)
	login := func(code string) (api.LoginResponse, int) {
		var lr api.LoginResponse
		c := call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "dana@vendor.com", Password: "correct horse battery", Code: code}, &lr)
		return lr, c
	}
	first, _ := login("")
	other, _ := login("")

	// Setup needs a dashboard session, not an API token.
	var pt api.APIToken
	call(t, ts, first.Token, "POST", "/api/v1/me/tokens", map[string]string{"name": "ci"}, &pt)
	if code := call(t, ts, pt.Token, "POST", "/api/v1/me/2fa/setup", nil, nil); code != 403 {
		t.Fatalf("setup with API token: %d", code)
	}
	var setup api.TwoFactorSetup
	call(t, ts, first.Token, "POST", "/api/v1/me/2fa/setup", nil, &setup)
	if setup.Secret == "" || len(setup.QRPNG) == 0 || setup.URI == "" {
		t.Fatalf("setup: %+v", setup)
	}
	if code := call(t, ts, first.Token, "POST", "/api/v1/me/2fa/enable", map[string]string{"code": "000000"}, nil); code != 400 {
		t.Fatalf("wrong confirmation code: %d", code)
	}
	code, _ := totp.Code(setup.Secret, totp.Step(clock))
	var rc api.RecoveryCodes
	if c := call(t, ts, first.Token, "POST", "/api/v1/me/2fa/enable", map[string]string{"code": code}, &rc); c != 200 || len(rc.Codes) != 10 {
		t.Fatalf("enable: %d %v", c, rc)
	}
	if c := call(t, ts, other.Token, "GET", "/api/v1/me", nil, nil); c != 401 {
		t.Fatalf("password-only session survived enabling 2FA: %d", c)
	}
	if c := call(t, ts, first.Token, "GET", "/api/v1/me", nil, nil); c != 200 {
		t.Fatalf("enabling session signed out: %d", c)
	}

	// Password alone now yields a challenge, not a session.
	lr, c := login("")
	if c != 200 || !lr.TwoFactorRequired || lr.Token != "" || lr.Challenge == "" {
		t.Fatalf("password step: %d %+v", c, lr)
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: "123456"}, nil); c != 401 {
		t.Fatalf("wrong code: %d", c)
	}
	// The code used to enable is spent; the next step's code works.
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: code}, nil); c != 401 {
		t.Fatalf("replayed enable code: %d", c)
	}
	tick()
	code, _ = totp.Code(setup.Secret, totp.Step(clock))
	var done api.LoginResponse
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: code}, &done); c != 200 || done.Token == "" {
		t.Fatalf("second step: %d", c)
	}
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: code}, nil); c != 401 {
		t.Fatalf("challenge reused: %d", c)
	}
	// One-step sign-in with the same code is a replay.
	if _, c := login(code); c != 401 {
		t.Fatalf("replay in one-step login: %d", c)
	}

	// Recovery codes work once, in any format.
	if lr, c := login(rc.Codes[0]); c != 200 || lr.Token == "" {
		t.Fatalf("recovery code: %d", c)
	}
	if _, c := login(rc.Codes[0]); c != 401 {
		t.Fatalf("recovery code reused: %d", c)
	}

	// A challenge allows five tries.
	lr, _ = login("")
	for i := 0; i < 5; i++ {
		call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: "999999"}, nil)
	}
	tick()
	code, _ = totp.Code(setup.Secret, totp.Step(clock))
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa", api.TwoFactorLoginRequest{Challenge: lr.Challenge, Code: code}, nil); c != 401 {
		t.Fatalf("challenge after 5 failures: %d", c)
	}

	// Lost phone: an admin resets it, which also signs the user out.
	var users []api.User
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	if !users[0].TwoFactor {
		t.Fatal("user list doesn't show 2FA")
	}
	call(t, ts, admin, "PATCH", "/api/v1/users/"+users[0].ID, map[string]any{"reset_two_factor": true}, nil)
	if c := call(t, ts, done.Token, "GET", "/api/v1/me", nil, nil); c != 401 {
		t.Fatalf("session survived 2FA reset: %d", c)
	}
	s.logins = loginLimiter{} // the failures above locked the email
	if lr, c := login(""); c != 200 || lr.TwoFactorRequired {
		t.Fatalf("after reset: %d %+v", c, lr)
	}
}

func TestRequireTwoFactorAndPasswordLoginOff(t *testing.T) {
	s, ts := setupWith(t, func(c *Config) { c.Require2FA = true })
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "op@vendor.com", Role: api.RoleOperator, Password: "operator-password"}, nil)
	var lr api.LoginResponse
	call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "op@vendor.com", Password: "operator-password"}, &lr)
	var me api.Me
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if !me.TwoFactorSetupRequired || me.SignInMethod != "password" {
		t.Fatalf("me: %+v", me)
	}
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, nil); c != 403 {
		t.Fatalf("used the fleet before setting up 2FA: %d", c)
	}
	var setup api.TwoFactorSetup
	call(t, ts, lr.Token, "POST", "/api/v1/me/2fa/setup", nil, &setup)
	code, _ := totp.Code(setup.Secret, totp.Step(s.now()))
	call(t, ts, lr.Token, "POST", "/api/v1/me/2fa/enable", map[string]string{"code": code}, nil)
	if c := call(t, ts, lr.Token, "GET", "/api/v1/devices", nil, nil); c != 200 {
		t.Fatalf("after setup: %d", c)
	}
	if c := call(t, ts, lr.Token, "POST", "/api/v1/me/2fa/disable", map[string]string{"code": code}, nil); c != 403 {
		t.Fatalf("turned off required 2FA: %d", c)
	}

	_, ts2 := setupWith(t, func(c *Config) { c.PasswordLogin = false })
	call(t, ts2, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "x@vendor.com", Role: api.RoleViewer, Password: "viewer-password"}, nil)
	if c := call(t, ts2, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "x@vendor.com", Password: "viewer-password"}, nil); c != 403 {
		t.Fatalf("password login while off: %d", c)
	}
	var ac api.AuthConfig
	call(t, ts2, "", "GET", "/api/v1/auth/config", nil, &ac)
	if ac.PasswordLogin || ac.SSO {
		t.Fatalf("auth config: %+v", ac)
	}
}
