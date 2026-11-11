package license

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (ed25519.PrivateKey, *Verifier) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	jwks, _ := json.Marshal(PublicJWKS(pub))
	v, err := NewVerifierFromJWKS(jwks, "com.acme.app")
	if err != nil {
		t.Fatal(err)
	}
	return priv, v
}

func claims(now time.Time) *Claims {
	return &Claims{Issuer: Issuer, Subject: "dev_1", Audience: "com.acme.app", IssuedAt: now.Unix(), NotBefore: now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(), ID: "lic_1", Device: DeviceClaims{Name: "kiosk", Channel: "stable", Labels: map[string]string{"customer": "acme"}}}
}

func TestIssueVerify(t *testing.T) {
	priv, v := setup(t)
	now := time.Now()
	tok, err := Issue(priv, KeyID(priv.Public().(ed25519.PublicKey)), claims(now))
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID() != "dev_1" || c.Device.Labels["customer"] != "acme" {
		t.Fatalf("claims: %+v", c)
	}
	if _, err := v.Verify(tok, now.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	other := NewVerifier("com.other.app", priv.Public().(ed25519.PublicKey))
	if _, err := other.Verify(tok, now); !errors.Is(err, ErrAudience) {
		t.Fatalf("audience: %v", err)
	}
	// Tampering with the payload breaks the signature.
	parts := strings.Split(tok, ".")
	forged := claims(now)
	forged.Subject = "dev_2"
	pb, _ := json.Marshal(forged)
	if _, err := v.Verify(parts[0]+"."+b64.EncodeToString(pb)+"."+parts[2], now); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered: %v", err)
	}
	// A different issuer key is rejected.
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	tok2, _ := Issue(priv2, KeyID(priv.Public().(ed25519.PublicKey)), claims(now))
	if _, err := v.Verify(tok2, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestDPoP(t *testing.T) {
	priv, v := setup(t)
	now := time.Now()
	appKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := claims(now)
	c.Confirmation = &Confirmation{JKT: Thumbprint(ECJWK(&appKey.PublicKey))}
	tok, _ := Issue(priv, KeyID(priv.Public().(ed25519.PublicKey)), c)
	vc, err := v.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	const u = "https://api.acme.com/v1/orders"
	proof, err := MakeDPoPProof(appKey, "POST", u, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyDPoP(vc, tok, proof, "POST", u+"?page=2", now); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	if err := v.VerifyDPoP(vc, tok, proof, "POST", u, now); err == nil || !strings.Contains(err.Error(), "replayed") {
		t.Fatalf("replay accepted: %v", err)
	}
	if err := v.VerifyDPoP(vc, tok, "", "POST", u, now); !errors.Is(err, ErrDPoPRequired) {
		t.Fatalf("missing proof: %v", err)
	}
	fresh := func() string { p, _ := MakeDPoPProof(appKey, "POST", u, tok, now); return p }
	if err := v.VerifyDPoP(vc, tok, fresh(), "GET", u, now); err == nil {
		t.Fatal("method mismatch accepted")
	}
	if err := v.VerifyDPoP(vc, tok, fresh(), "POST", "https://api.acme.com/v1/admin", now); err == nil {
		t.Fatal("URL mismatch accepted")
	}
	if err := v.VerifyDPoP(vc, tok, fresh(), "POST", u, now.Add(10*time.Minute)); err == nil {
		t.Fatal("stale proof accepted")
	}
	// A thief with the token but not the device key cannot make proofs.
	thief, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	stolen, _ := MakeDPoPProof(thief, "POST", u, tok, now)
	if err := v.VerifyDPoP(vc, tok, stolen, "POST", u, now); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("thief proof accepted: %v", err)
	}
	// A proof for a different token is rejected.
	other, _ := MakeDPoPProof(appKey, "POST", u, tok+"x", now)
	if err := v.VerifyDPoP(vc, tok, other, "POST", u, now); err == nil {
		t.Fatal("proof for other token accepted")
	}
}

func TestThumbprintRFC7638(t *testing.T) {
	// A published P-256 example key and its RFC 7638 thumbprint (from the DPoP
	// RFC's examples). Optional members such as kid must not affect it.
	j := JWK{Kty: "EC", Crv: "P-256", X: "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU", Y: "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0", Kid: "ignored"}
	if got := Thumbprint(j); got != "oKIywvGUpTVTyxMQ3bwIIeQUudfr_CkLMjCE19ECD-U" {
		t.Fatalf("thumbprint %s", got)
	}
}

func TestMiddleware(t *testing.T) {
	priv, v := setup(t)
	now := time.Now()
	appKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c := claims(now)
	c.Confirmation = &Confirmation{JKT: Thumbprint(ECJWK(&appKey.PublicKey))}
	tok, _ := Issue(priv, KeyID(priv.Public().(ed25519.PublicKey)), c)
	h := v.Middleware("https://api.acme.com", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := ClaimsFromContext(r.Context())
		w.Write([]byte(c.Device.Labels["customer"]))
	}))
	do := func(auth, dpop string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://internal:8080/v1/me?x=1", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if dpop != "" {
			req.Header.Set("DPoP", dpop)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	proof, _ := MakeDPoPProof(appKey, "GET", "https://api.acme.com/v1/me", tok, now)
	if rec := do("DPoP "+tok, proof); rec.Code != 200 || rec.Body.String() != "acme" {
		t.Fatalf("valid request: %d %s", rec.Code, rec.Body)
	}
	if rec := do("", ""); rec.Code != 401 {
		t.Fatalf("no license: %d", rec.Code)
	}
	if rec := do("Bearer "+tok, ""); rec.Code != 401 {
		t.Fatalf("bound license without proof: %d", rec.Code)
	}
}
