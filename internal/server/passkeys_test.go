package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/nasi-lemak/armada/internal/api"
)

// softKey is a software authenticator: it answers WebAuthn create/get
// options the way a browser plus a passkey provider would.
type softKey struct {
	key        *ecdsa.PrivateKey
	id         []byte
	userHandle []byte
	counter    uint32
	rpID       string
	origin     string
	uv         bool // performs user verification (biometric/PIN)
}

func newSoftKey(rpID, origin string) *softKey {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	id := make([]byte, 16)
	rand.Read(id)
	return &softKey{key: k, id: id, rpID: rpID, origin: origin, uv: true}
}

var b64u = base64.RawURLEncoding

func (k *softKey) authData(extra []byte, attested bool) []byte {
	rp := sha256.Sum256([]byte(k.rpID))
	flags := byte(0x01 | 0x08 | 0x10) // user present, backup eligible + state (a synced passkey)
	if k.uv {
		flags |= 0x04
	}
	if attested {
		flags |= 0x40
	}
	k.counter++
	ad := append(rp[:], flags)
	ad = binary.BigEndian.AppendUint32(ad, k.counter)
	return append(ad, extra...)
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

type wireOpts struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	} `json:"publicKey"`
}

func (k *softKey) create(t *testing.T, raw json.RawMessage) json.RawMessage {
	var o wireOpts
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	k.userHandle, _ = b64u.DecodeString(o.PublicKey.User.ID)
	cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: k.key.X.FillBytes(make([]byte, 32)), -3: k.key.Y.FillBytes(make([]byte, 32))})
	att := make([]byte, 16) // AAGUID
	att = binary.BigEndian.AppendUint16(att, uint16(len(k.id)))
	att = append(append(att, k.id...), cose...)
	k.counter = 0
	ao, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": k.authData(att, true)})
	out, _ := json.Marshal(map[string]any{"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(k.clientData("webauthn.create", o.PublicKey.Challenge)),
			"attestationObject": b64u.EncodeToString(ao)}, "clientExtensionResults": map[string]any{}})
	return out
}

func (k *softKey) get(t *testing.T, raw json.RawMessage) json.RawMessage {
	var o wireOpts
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	ad := k.authData(nil, false)
	cd := k.clientData("webauthn.get", o.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, _ := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	out, _ := json.Marshal(map[string]any{"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(cd), "authenticatorData": b64u.EncodeToString(ad),
			"signature": b64u.EncodeToString(sig), "userHandle": b64u.EncodeToString(k.userHandle)}, "clientExtensionResults": map[string]any{}})
	return out
}

type ceremonyResp struct {
	Ceremony string          `json:"ceremony"`
	Options  json.RawMessage `json:"options"`
}

func TestPasskeys(t *testing.T) {
	s, ts := setupWith(t, func(c *Config) { c.Require2FA = true })
	origin := "http://localhost:" + ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	s.cfg.Android.PublicURL = origin // passkeys are bound to this origin's host
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	call(t, ts, admin, "POST", "/api/v1/users", api.CreateUserRequest{Email: "dana@vendor.com", Role: api.RoleAdmin, Password: "correct horse battery"}, nil)
	login := func() api.LoginResponse {
		var lr api.LoginResponse
		call(t, ts, "", "POST", "/api/v1/auth/login", api.LoginRequest{Email: "dana@vendor.com", Password: "correct horse battery"}, &lr)
		return lr
	}
	sess := login().Token
	other := login().Token

	// Registration needs a recent sign-in.
	clock = clock.Add(20 * time.Minute)
	if c := call(t, ts, sess, "POST", "/api/v1/me/passkeys/register/begin", nil, nil); c != 403 {
		t.Fatalf("stale session registered: %d", c)
	}
	sess = login().Token
	key := newSoftKey("localhost", origin)
	var cr ceremonyResp
	call(t, ts, sess, "POST", "/api/v1/me/passkeys/register/begin", nil, &cr)
	var pk api.Passkey
	if c := call(t, ts, sess, "POST", "/api/v1/me/passkeys/register/finish", api.PasskeyFinish{Ceremony: cr.Ceremony, Name: "MacBook", Credential: key.create(t, cr.Options)}, &pk); c != 201 || !pk.Synced || pk.Name != "MacBook" {
		t.Fatalf("register: %d %+v", c, pk)
	}
	if c := call(t, ts, other, "GET", "/api/v1/me", nil, nil); c != 401 {
		t.Fatalf("password-only session survived the first second factor: %d", c)
	}
	var me api.Me
	call(t, ts, sess, "GET", "/api/v1/me", nil, &me)
	if me.User.Passkeys != 1 || me.TwoFactorSetupRequired {
		t.Fatalf("a passkey should satisfy -require-2fa: %+v", me)
	}

	// Passwordless sign-in.
	passwordless := func(k *softKey) (api.LoginResponse, int) {
		var cr ceremonyResp
		call(t, ts, "", "POST", "/api/v1/auth/passkey/begin", nil, &cr)
		var lr api.LoginResponse
		c := call(t, ts, "", "POST", "/api/v1/auth/passkey/finish", api.PasskeyFinish{Ceremony: cr.Ceremony, Credential: k.get(t, cr.Options)}, &lr)
		return lr, c
	}
	lr, c := passwordless(key)
	if c != 200 || lr.Token == "" {
		t.Fatalf("passkey sign-in: %d", c)
	}
	call(t, ts, lr.Token, "GET", "/api/v1/me", nil, &me)
	if me.SignInMethod != "passkey" {
		t.Fatalf("method: %s", me.SignInMethod)
	}
	// Another site can't use it: the browser would put its own origin in.
	phished := *key
	phished.origin = "https://armada-login.evil.example"
	if _, c := passwordless(&phished); c != 401 {
		t.Fatalf("assertion for another origin accepted: %d", c)
	}
	// Passwordless needs user verification (it's both factors at once).
	noUV := *key
	noUV.uv = false
	if _, c := passwordless(&noUV); c != 401 {
		t.Fatalf("passwordless without user verification: %d", c)
	}
	// A ceremony is single-use.
	call(t, ts, "", "POST", "/api/v1/auth/passkey/begin", nil, &cr)
	resp := key.get(t, cr.Options)
	call(t, ts, "", "POST", "/api/v1/auth/passkey/finish", api.PasskeyFinish{Ceremony: cr.Ceremony, Credential: resp}, nil)
	if c := call(t, ts, "", "POST", "/api/v1/auth/passkey/finish", api.PasskeyFinish{Ceremony: cr.Ceremony, Credential: resp}, nil); c != 401 {
		t.Fatalf("replayed ceremony: %d", c)
	}
	// A cloned security key (counter goes backwards) is refused.
	clone := *key
	clone.counter = 0
	if _, c := passwordless(&clone); c != 401 {
		t.Fatalf("cloned key accepted: %d", c)
	}

	// After a password, the passkey is the second step (no UV needed).
	ch := login()
	if !ch.TwoFactorRequired || len(ch.TwoFactorMethods) != 1 || ch.TwoFactorMethods[0] != "passkey" {
		t.Fatalf("password step: %+v", ch)
	}
	call(t, ts, "", "POST", "/api/v1/auth/2fa/passkey/begin", api.TwoFactorLoginRequest{Challenge: ch.Challenge}, &cr)
	key2fa := *key
	key2fa.uv = false
	if c := call(t, ts, "", "POST", "/api/v1/auth/2fa/passkey/finish", api.PasskeyFinish{Ceremony: cr.Ceremony, Challenge: ch.Challenge, Credential: key2fa.get(t, cr.Options)}, &lr); c != 200 || lr.Token == "" {
		t.Fatalf("passkey second step: %d", c)
	}
	key.counter = key2fa.counter

	// The last passkey can't be removed while two-factor is required.
	var list []api.Passkey
	call(t, ts, lr.Token, "GET", "/api/v1/me/passkeys", nil, &list)
	if c := call(t, ts, lr.Token, "DELETE", "/api/v1/me/passkeys/"+list[0].ID, nil, nil); c != 403 {
		t.Fatalf("removed the only second factor: %d", c)
	}
	// Lost device: an admin reset removes passkeys too.
	var users []api.User
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	call(t, ts, admin, "PATCH", "/api/v1/users/"+users[0].ID, map[string]any{"reset_two_factor": true}, nil)
	if _, c := passwordless(key); c != 401 {
		t.Fatalf("passkey works after reset: %d", c)
	}
	call(t, ts, admin, "GET", "/api/v1/users", nil, &users)
	if users[0].Passkeys != 0 {
		t.Fatalf("passkey count after reset: %d", users[0].Passkeys)
	}
}
