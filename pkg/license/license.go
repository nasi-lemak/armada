// Package license issues and verifies Armada device licenses.
//
// A license is a standard JWT (RFC 7519) signed with EdDSA (Ed25519). It says
// "this app may run on this enrolled device until exp". Armada issues one
// only to devices that proved, with a hardware-backed Android Keystore key,
// that they are the physical device that was enrolled.
//
// Your backend should require a valid license on every request:
//
//	v, _ := license.NewVerifierFromJWKSURL(ctx, "https://fleet.example.com/api/v1/license/jwks", "com.acme.app")
//	token := license.TokenFromAuthorization(r.Header.Get("Authorization"))
//	claims, err := v.Verify(token, time.Now())
//
// If the license carries a confirmation key (cnf.jkt), also require a DPoP
// proof (RFC 9449) signed by the app's own non-exportable key, so a copied
// token is useless on another device:
//
//	err = v.VerifyDPoP(claims, token, r.Header.Get("DPoP"), r.Method, requestURL, time.Now())
//
// Or wrap your handlers with Verifier.Middleware, which does both.
//
// Because licenses are ordinary EdDSA JWTs with a JWKS endpoint, backends in
// other languages can verify them with any JWT library that supports EdDSA.
package license

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Issuer is the "iss" claim of every Armada license.
const Issuer = "armada"

// TokenType is the JWT "typ" header of a license.
const TokenType = "armada-license+jwt"

// Claims are the contents of a license.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"` // device ID
	Audience  string `json:"aud"` // Android package name of the licensed app
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf"`
	ExpiresAt int64  `json:"exp"`
	ID        string `json:"jti"`
	// Device describes the device the license is bound to.
	Device DeviceClaims `json:"armada"`
	// Confirmation binds the license to the app's own key (RFC 7800 / RFC 9449).
	Confirmation *Confirmation `json:"cnf,omitempty"`
}

// DeviceClaims carries fleet metadata your backend can use, such as a
// customer label to scope data.
type DeviceClaims struct {
	Name    string            `json:"device_name"`
	Channel string            `json:"channel"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// Confirmation holds the JWK SHA-256 thumbprint (RFC 7638) of the key that
// must sign DPoP proofs.
type Confirmation struct {
	JKT string `json:"jkt"`
}

// DeviceID returns the licensed device.
func (c *Claims) DeviceID() string { return c.Subject }

type header struct {
	Alg string          `json:"alg"`
	Typ string          `json:"typ"`
	Kid string          `json:"kid,omitempty"`
	JWK json.RawMessage `json:"jwk,omitempty"`
}

var b64 = base64.RawURLEncoding

// Issue signs claims with key. kid identifies the key in the JWKS.
func Issue(key ed25519.PrivateKey, kid string, c *Claims) (string, error) {
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: TokenType, Kid: kid})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	return input + "." + b64.EncodeToString(ed25519.Sign(key, []byte(input))), nil
}

// KeyID derives a stable key ID from a public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return b64.EncodeToString(sum[:8])
}

// JWK is a public key in JSON Web Key form.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y,omitempty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// PublicJWKS returns the key set a backend needs to verify licenses.
func PublicJWKS(pubs ...ed25519.PublicKey) JWKS {
	var s JWKS
	for _, p := range pubs {
		s.Keys = append(s.Keys, JWK{Kty: "OKP", Crv: "Ed25519", X: b64.EncodeToString(p), Kid: KeyID(p), Use: "sig", Alg: "EdDSA"})
	}
	return s
}

// Verifier checks licenses for one app.
type Verifier struct {
	keys     map[string]ed25519.PublicKey
	audience string
	// Leeway tolerates clock skew between the Armada server and your backend.
	Leeway time.Duration
	// DPoPMaxAge bounds how old a DPoP proof may be (default 2 minutes).
	DPoPMaxAge time.Duration
	// replay rejects reused DPoP proof IDs within DPoPMaxAge.
	replay sync.Map
}

// NewVerifier verifies licenses for audience (your app's package name)
// signed by any of keys.
func NewVerifier(audience string, keys ...ed25519.PublicKey) *Verifier {
	v := &Verifier{keys: map[string]ed25519.PublicKey{}, audience: audience, Leeway: 30 * time.Second, DPoPMaxAge: 2 * time.Minute}
	for _, k := range keys {
		v.keys[KeyID(k)] = k
	}
	return v
}

// NewVerifierFromJWKS parses a JWKS document.
func NewVerifierFromJWKS(jwks []byte, audience string) (*Verifier, error) {
	var set JWKS
	if err := json.Unmarshal(jwks, &set); err != nil {
		return nil, fmt.Errorf("parse JWKS: %w", err)
	}
	var keys []ed25519.PublicKey
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		x, err := b64.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("JWKS contains a malformed Ed25519 key")
		}
		keys = append(keys, ed25519.PublicKey(x))
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no Ed25519 keys")
	}
	return NewVerifier(audience, keys...), nil
}

// NewVerifierFromJWKSURL fetches the JWKS from the Armada server
// (GET /api/v1/license/jwks). Cache the verifier; the key rarely changes.
func NewVerifierFromJWKSURL(ctx context.Context, jwksURL, audience string) (*Verifier, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return NewVerifierFromJWKS(b, audience)
}

// Errors returned by Verify.
var (
	ErrMalformed = errors.New("license is malformed")
	ErrSignature = errors.New("license signature is invalid")
	ErrExpired   = errors.New("license has expired")
	ErrAudience  = errors.New("license is for a different app")
)

// Verify checks a license's signature, issuer, audience and validity window.
func (v *Verifier) Verify(token string, now time.Time) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	var h header
	if err := decodeJSON(parts[0], &h); err != nil || h.Alg != "EdDSA" || h.Typ != TokenType {
		return nil, ErrMalformed
	}
	key, ok := v.keys[h.Kid]
	if !ok {
		return nil, fmt.Errorf("%w: unknown key id %q", ErrSignature, h.Kid)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrSignature
	}
	var c Claims
	if err := decodeJSON(parts[1], &c); err != nil {
		return nil, ErrMalformed
	}
	if c.Issuer != Issuer || c.Subject == "" {
		return nil, ErrMalformed
	}
	if v.audience != "" && c.Audience != v.audience {
		return nil, ErrAudience
	}
	t := now.Unix()
	lw := int64(v.Leeway.Seconds())
	if t+lw < c.NotBefore || t-lw >= c.ExpiresAt {
		return nil, ErrExpired
	}
	return &c, nil
}

func decodeJSON(seg string, v any) error {
	b, err := b64.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ---- DPoP (RFC 9449) ----

// DPoP errors.
var (
	ErrDPoPRequired = errors.New("license is bound to a device key; a DPoP proof is required")
	ErrDPoPInvalid  = errors.New("DPoP proof is invalid")
)

type dpopClaims struct {
	JTI string `json:"jti"`
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	ATH string `json:"ath"`
}

// VerifyDPoP checks the DPoP proof for one request. Call it after Verify.
// requestURL is the full URL the client called (scheme, host and path).
// If the license is not key-bound, VerifyDPoP accepts an empty proof.
func (v *Verifier) VerifyDPoP(c *Claims, token, proof, method, requestURL string, now time.Time) error {
	if c.Confirmation == nil || c.Confirmation.JKT == "" {
		return nil
	}
	if proof == "" {
		return ErrDPoPRequired
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		return fmt.Errorf("%w: malformed", ErrDPoPInvalid)
	}
	var h header
	if err := decodeJSON(parts[0], &h); err != nil || h.Typ != "dpop+jwt" || h.Alg != "ES256" || len(h.JWK) == 0 {
		return fmt.Errorf("%w: bad header", ErrDPoPInvalid)
	}
	var jwk JWK
	if err := json.Unmarshal(h.JWK, &jwk); err != nil {
		return fmt.Errorf("%w: bad jwk", ErrDPoPInvalid)
	}
	pub, err := ecPublicKey(jwk)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDPoPInvalid, err)
	}
	if Thumbprint(jwk) != c.Confirmation.JKT {
		return fmt.Errorf("%w: signed by a key the license is not bound to", ErrDPoPInvalid)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return fmt.Errorf("%w: bad signature encoding", ErrDPoPInvalid)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return fmt.Errorf("%w: bad signature", ErrDPoPInvalid)
	}
	var dc dpopClaims
	if err := decodeJSON(parts[1], &dc); err != nil || dc.JTI == "" {
		return fmt.Errorf("%w: bad claims", ErrDPoPInvalid)
	}
	if !strings.EqualFold(dc.HTM, method) {
		return fmt.Errorf("%w: method mismatch", ErrDPoPInvalid)
	}
	if normalizeHTU(dc.HTU) != normalizeHTU(requestURL) {
		return fmt.Errorf("%w: URL mismatch", ErrDPoPInvalid)
	}
	maxAge := v.DPoPMaxAge
	if age := now.Sub(time.Unix(dc.IAT, 0)); age > maxAge || age < -v.Leeway {
		return fmt.Errorf("%w: proof is too old or from the future", ErrDPoPInvalid)
	}
	ath := sha256.Sum256([]byte(token))
	if dc.ATH != b64.EncodeToString(ath[:]) {
		return fmt.Errorf("%w: proof is for a different token", ErrDPoPInvalid)
	}
	key := c.Confirmation.JKT + "/" + dc.JTI
	if prev, loaded := v.replay.LoadOrStore(key, now); loaded && now.Sub(prev.(time.Time)) <= maxAge {
		return fmt.Errorf("%w: proof was replayed", ErrDPoPInvalid)
	}
	v.pruneReplay(now)
	return nil
}

func (v *Verifier) pruneReplay(now time.Time) {
	v.replay.Range(func(k, val any) bool {
		if now.Sub(val.(time.Time)) > 2*v.DPoPMaxAge {
			v.replay.Delete(k)
		}
		return true
	})
}

// normalizeHTU compares URLs without query and fragment, per RFC 9449 §4.3.
func normalizeHTU(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.RawQuery, u.Fragment = "", ""
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}

func ecPublicKey(j JWK) (*ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, errors.New("DPoP key must be an EC P-256 JWK")
	}
	x, err1 := b64.DecodeString(j.X)
	y, err2 := b64.DecodeString(j.Y)
	if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("malformed EC JWK")
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, errors.New("EC point is not on P-256")
	}
	return pub, nil
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of an EC P-256 JWK.
func Thumbprint(j JWK) string {
	canon := `{"crv":"` + j.Crv + `","kty":"` + j.Kty + `","x":"` + j.X + `","y":"` + j.Y + `"}`
	sum := sha256.Sum256([]byte(canon))
	return b64.EncodeToString(sum[:])
}

// ECJWK converts an EC P-256 public key to a JWK.
func ECJWK(pub *ecdsa.PublicKey) JWK {
	return JWK{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(pad32(pub.X.Bytes())), Y: b64.EncodeToString(pad32(pub.Y.Bytes()))}
}

func pad32(b []byte) []byte {
	if len(b) >= 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// ParseECJWK validates an EC P-256 JWK.
func ParseECJWK(j JWK) (*ecdsa.PublicKey, error) { return ecPublicKey(j) }

// MakeDPoPProof creates a DPoP proof for one request. The Android client
// library does the same with a Keystore key; this is for Go clients and tests.
func MakeDPoPProof(key *ecdsa.PrivateKey, method, requestURL, token string, now time.Time) (string, error) {
	jwk, err := json.Marshal(ECJWK(&key.PublicKey))
	if err != nil {
		return "", err
	}
	h, err := json.Marshal(header{Alg: "ES256", Typ: "dpop+jwt", JWK: jwk})
	if err != nil {
		return "", err
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	ath := sha256.Sum256([]byte(token))
	p, err := json.Marshal(dpopClaims{JTI: b64.EncodeToString(jti), HTM: method, HTU: requestURL, IAT: now.Unix(), ATH: b64.EncodeToString(ath[:])})
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := append(pad32(r.Bytes()), pad32(s.Bytes())...)
	return input + "." + b64.EncodeToString(sig), nil
}

// TokenFromAuthorization extracts the license from an Authorization header
// using either the "DPoP" scheme (what the Android client sends) or "Bearer".
func TokenFromAuthorization(h string) string {
	for _, scheme := range []string{"DPoP ", "Bearer "} {
		if len(h) > len(scheme) && strings.EqualFold(h[:len(scheme)], scheme) {
			return strings.TrimSpace(h[len(scheme):])
		}
	}
	return ""
}

type ctxKey struct{}

// ClaimsFromContext returns the license claims Middleware verified.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*Claims)
	return c, ok
}

// Middleware rejects requests without a valid license (and DPoP proof, when
// the license is key-bound) with 401. publicBaseURL is how clients reach your
// API (e.g. "https://api.acme.com"), needed to check the DPoP URL behind a
// proxy. Verified claims are available via ClaimsFromContext.
func (v *Verifier) Middleware(publicBaseURL string, next http.Handler) http.Handler {
	base := strings.TrimRight(publicBaseURL, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		token := TokenFromAuthorization(r.Header.Get("Authorization"))
		c, err := v.Verify(token, now)
		if err == nil {
			err = v.VerifyDPoP(c, token, r.Header.Get("DPoP"), r.Method, base+r.URL.Path, now)
		}
		if err != nil {
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_token"`)
			http.Error(w, "license required: "+err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	})
}
