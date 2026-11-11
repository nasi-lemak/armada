package server

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/attestation"
	"github.com/nasi-lemak/armada/internal/rollout"
	"github.com/nasi-lemak/armada/internal/store"
	"github.com/nasi-lemak/armada/pkg/license"
)

// ---- challenges ----

const (
	challengeTTL  = 5 * time.Minute
	maxChallenges = 100_000
)

// challengeStore hands out single-use random nonces. Challenges are not
// secret; they only stop old attestations and signatures from being replayed.
type challengeStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newChallengeStore() *challengeStore { return &challengeStore{m: map[string]time.Time{}} }

func (c *challengeStore) issue(now time.Time) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ch := base64.StdEncoding.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxChallenges {
		for k, exp := range c.m {
			if now.After(exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= maxChallenges {
			return "", errors.New("too many outstanding challenges")
		}
	}
	c.m[ch] = now.Add(challengeTTL)
	return ch, nil
}

func (c *challengeStore) consume(ch string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.m[ch]
	delete(c.m, ch)
	return ok && now.Before(exp)
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	ch, err := s.challenges.issue(s.now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.ChallengeResponse{Challenge: ch, ExpiresIn: int(challengeTTL.Seconds())})
}

// ---- attested enrollment ----

var errAttestation = errors.New("attestation rejected")

// verifyEnrollment checks the attestation in an enrollment request. It returns
// the attestation summary and the attested public key (base64 PKIX), both
// empty if the request carries no attestation and none is required.
func (s *Server) verifyEnrollment(ctx context.Context, req *api.EnrollRequest) (*api.DeviceAttestation, string, error) {
	isAndroid := strings.EqualFold(req.Info.OS, "android")
	ev := req.Attestation
	if ev == nil {
		if isAndroid && s.cfg.Android.RequireAttestation {
			return nil, "", fmt.Errorf("%w: Android devices must enroll with a hardware key attestation", errAttestation)
		}
		return nil, "", nil
	}
	if !s.challenges.consume(ev.Challenge, s.now()) {
		return nil, "", fmt.Errorf("%w: unknown or expired challenge", errAttestation)
	}
	challenge, err := base64.StdEncoding.DecodeString(ev.Challenge)
	if err != nil {
		return nil, "", fmt.Errorf("%w: bad challenge encoding", errAttestation)
	}
	chain := make([][]byte, 0, len(ev.Chain))
	for _, c := range ev.Chain {
		der, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			return nil, "", fmt.Errorf("%w: bad certificate encoding", errAttestation)
		}
		chain = append(chain, der)
	}
	policy := attestation.Policy{
		Challenge:           challenge,
		MinSecurityLevel:    attestation.TrustedEnvironment,
		RequireVerifiedBoot: s.cfg.Android.RequireVerifiedBoot,
	}
	if s.cfg.Android.RequireStrongBox {
		policy.MinSecurityLevel = attestation.StrongBox
	}
	if s.cfg.Android.AgentPackage != "" {
		policy.AllowedPackages = []string{s.cfg.Android.AgentPackage}
	}
	// If the agent app is registered, its key must also come from a build
	// signed with your certificate, not a look-alike app with the same name.
	if app, err := s.store.GetApp(ctx, s.cfg.Android.AgentPackage); err == nil && len(app.Signers) > 0 {
		policy.AllowedSigners = app.Signers
	}
	res, err := s.verifier.Verify(chain, policy)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errAttestation, err)
	}
	summary := &api.DeviceAttestation{
		SecurityLevel: res.SecurityLevel.String(),
		BootState:     res.RootOfTrust.VerifiedBootState.String(),
		DeviceLocked:  res.RootOfTrust.DeviceLocked,
		OSPatchLevel:  res.OSPatchLevel,
		VerifiedAt:    s.now(),
	}
	return summary, base64.StdEncoding.EncodeToString(res.PublicKeyDER), nil
}

// ---- licensing ----

func (s *Server) handleLicense(w http.ResponseWriter, r *http.Request, dev *api.Device) {
	var req api.LicenseRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	now := s.now()
	if dev.LicenseRevoked {
		writeError(w, http.StatusForbidden, "licenses for this device have been revoked")
		return
	}
	app, err := s.store.GetApp(ctx, req.AppPackage)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !app.Licensing) {
		writeError(w, http.StatusForbidden, "app is not registered for licensing")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	signer := attestation.NormalizeDigest(req.AppSigner)
	if !containsDigest(app.Signers, signer) {
		writeError(w, http.StatusForbidden, "app signing certificate is not registered")
		return
	}
	hwKey, err := s.store.DeviceHardwareKey(ctx, dev.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if hwKey == "" {
		writeError(w, http.StatusForbidden, "device has no attested hardware key; it must enroll with key attestation")
		return
	}
	if !s.challenges.consume(req.Challenge, now) {
		writeError(w, http.StatusForbidden, "unknown or expired challenge")
		return
	}
	jkt := ""
	if len(req.AppKeyJWK) > 0 {
		var jwk license.JWK
		if err := json.Unmarshal(req.AppKeyJWK, &jwk); err != nil {
			writeError(w, http.StatusBadRequest, "malformed app_key_jwk")
			return
		}
		if _, err := license.ParseECJWK(jwk); err != nil {
			writeError(w, http.StatusBadRequest, "app_key_jwk: "+err.Error())
			return
		}
		jkt = license.Thumbprint(jwk)
	}
	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		writeError(w, http.StatusBadRequest, "malformed signature")
		return
	}
	input := api.LicenseSigningInput(dev.ID, req.Challenge, req.AppPackage, req.AppSigner, jkt)
	if err := verifyHardwareSignature(hwKey, input, sig); err != nil {
		s.log.Warn("license request with bad hardware signature", "device", dev.ID, "err", err)
		writeError(w, http.StatusForbidden, "hardware key signature is invalid")
		return
	}
	ttl := time.Duration(app.LicenseTTLSeconds) * time.Second
	jti := make([]byte, 12)
	if _, err := rand.Read(jti); err != nil {
		s.internalError(w, err)
		return
	}
	c := &license.Claims{
		Issuer:    license.Issuer,
		Subject:   dev.ID,
		Audience:  app.Package,
		IssuedAt:  now.Unix(),
		NotBefore: now.Add(-time.Minute).Unix(),
		ExpiresAt: now.Add(ttl).Unix(),
		ID:        "lic_" + hex.EncodeToString(jti),
		Device:    license.DeviceClaims{Name: dev.Name, Channel: dev.Channel, Labels: dev.Labels},
	}
	if jkt != "" {
		c.Confirmation = &license.Confirmation{JKT: jkt}
	}
	pub := s.licenseKey.Public().(ed25519.PublicKey)
	tok, err := license.Issue(s.licenseKey, license.KeyID(pub), c)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.LicenseResponse{License: tok, ExpiresAt: time.Unix(c.ExpiresAt, 0).UTC()})
}

func verifyHardwareSignature(pkixB64 string, msg, sig []byte) error {
	der, err := base64.StdEncoding.DecodeString(pkixB64)
	if err != nil {
		return err
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(msg)
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(k, digest[:], sig) {
			return errors.New("ECDSA signature mismatch")
		}
		return nil
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig)
	}
	return fmt.Errorf("unsupported hardware key type %T", pub)
}

func containsDigest(list []string, d string) bool {
	for _, x := range list {
		if attestation.NormalizeDigest(x) == d {
			return true
		}
	}
	return false
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, license.PublicJWKS(s.licenseKey.Public().(ed25519.PublicKey)))
}

// ---- apps ----

var (
	androidPackageRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$`)
	digestRe         = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(apps))
}

func (s *Server) handlePutApp(w http.ResponseWriter, r *http.Request) {
	pkg := r.PathValue("package")
	if !androidPackageRe.MatchString(pkg) || len(pkg) > 255 {
		writeError(w, http.StatusBadRequest, "invalid Android package name")
		return
	}
	var req api.App
	if !readJSON(w, r, &req) {
		return
	}
	now := s.now()
	app := &api.App{Package: pkg, Name: truncate(strings.TrimSpace(req.Name), 128), Licensing: req.Licensing,
		LicenseTTLSeconds: req.LicenseTTLSeconds, CreatedAt: now, UpdatedAt: now}
	for _, d := range req.Signers {
		n := attestation.NormalizeDigest(d)
		if !digestRe.MatchString(n) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("signer %q is not a SHA-256 certificate digest (64 hex characters)", d))
			return
		}
		if !containsDigest(app.Signers, n) {
			app.Signers = append(app.Signers, n)
		}
	}
	if len(app.Signers) == 0 {
		writeError(w, http.StatusBadRequest, "at least one signing certificate digest is required")
		return
	}
	switch {
	case app.LicenseTTLSeconds == 0:
		app.LicenseTTLSeconds = 3600
	case app.LicenseTTLSeconds < 60 || app.LicenseTTLSeconds > 30*86400:
		writeError(w, http.StatusBadRequest, "license_ttl_seconds must be between 60 and 2592000")
		return
	}
	if old, err := s.store.GetApp(r.Context(), pkg); err == nil {
		app.CreatedAt = old.CreatedAt
	}
	if err := s.store.UpsertApp(r.Context(), app); err != nil {
		s.internalError(w, err)
		return
	}
	s.event(r.Context(), "app.updated", "", "", fmt.Sprintf("app %s registered (licensing %v, %d signer(s))", pkg, app.Licensing, len(app.Signers)))
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteApp(r.Context(), r.PathValue("package")); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "app not found")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// managedPackages lists the Android packages whose versions agents report.
func (s *Server) managedPackages(r *http.Request) []string {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.Package)
	}
	return out
}

// ---- provisioning ----

type provisioningRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	WifiSSID        string `json:"wifi_ssid,omitempty"`
	WifiPassword    string `json:"wifi_password,omitempty"`
	WifiSecurity    string `json:"wifi_security,omitempty"` // WPA | WEP | NONE
}

type provisioningResponse struct {
	Payload     string   `json:"payload"`
	QRPNG       string   `json:"qr_png"` // base64 PNG
	DownloadURL string   `json:"download_url"`
	Warnings    []string `json:"warnings,omitempty"`
}

// handleProvisioning builds the QR code scanned on a factory-reset device's
// welcome screen (tap six times) to install the agent as Device Owner.
func (s *Server) handleProvisioning(w http.ResponseWriter, r *http.Request) {
	var req provisioningRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !strings.HasPrefix(req.EnrollmentToken, "et_") {
		writeError(w, http.StatusBadRequest, "enrollment_token is required (create one first; it is shown only once)")
		return
	}
	ctx := r.Context()
	pkg := s.cfg.Android.AgentPackage
	app, err := s.store.GetApp(ctx, pkg)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "register the Android agent ("+pkg+") under Apps with its signing certificate digest first")
		return
	} else if err != nil {
		s.internalError(w, err)
		return
	}
	if _, err := s.newestRelease(r, pkg, "android"); err != nil {
		writeError(w, http.StatusConflict, "upload the Android agent APK as a release of package "+pkg+" (os=android) first")
		return
	}
	sum, err := hex.DecodeString(app.Signers[0])
	if err != nil {
		s.internalError(w, err)
		return
	}
	base := s.publicURL(r)
	download := base + "/api/v1/agent/download?package=" + pkg + "&os=android"
	component := pkg + "/" + s.cfg.Android.AdminReceiver
	if strings.HasPrefix(s.cfg.Android.AdminReceiver, ".") {
		component = pkg + "/" + pkg + s.cfg.Android.AdminReceiver
	}
	payload := map[string]any{
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME":            component,
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_PACKAGE_DOWNLOAD_LOCATION": download,
		"android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM":        base64.RawURLEncoding.EncodeToString(sum),
		"android.app.extra.PROVISIONING_LEAVE_ALL_SYSTEM_APPS_ENABLED":          true,
		"android.app.extra.PROVISIONING_SKIP_ENCRYPTION":                        false,
		"android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE": map[string]string{
			"server_url":       base,
			"enrollment_token": req.EnrollmentToken,
		},
	}
	if req.WifiSSID != "" {
		payload["android.app.extra.PROVISIONING_WIFI_SSID"] = req.WifiSSID
		sec := strings.ToUpper(req.WifiSecurity)
		if sec == "" {
			sec = "WPA"
		}
		payload["android.app.extra.PROVISIONING_WIFI_SECURITY_TYPE"] = sec
		if req.WifiPassword != "" {
			payload["android.app.extra.PROVISIONING_WIFI_PASSWORD"] = req.WifiPassword
		}
	}
	js, err := json.Marshal(payload)
	if err != nil {
		s.internalError(w, err)
		return
	}
	png, err := qrcode.Encode(string(js), qrcode.Medium, 512)
	if err != nil {
		s.internalError(w, err)
		return
	}
	res := provisioningResponse{Payload: string(js), QRPNG: base64.StdEncoding.EncodeToString(png), DownloadURL: download}
	if !strings.HasPrefix(base, "https://") {
		res.Warnings = append(res.Warnings, "The server URL is not HTTPS. Use HTTPS in production so the agent download and enrollment token are protected in transit.")
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.Android.PublicURL != "" {
		return strings.TrimRight(s.cfg.Android.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = fh
	}
	return scheme + "://" + host
}

// newestRelease finds the highest version of pkg for a platform.
func (s *Server) newestRelease(r *http.Request, pkg, osName string) (*api.Release, error) {
	rels, err := s.store.ListReleases(r.Context(), pkg)
	if err != nil {
		return nil, err
	}
	dev := &api.Device{Info: api.DeviceInfo{OS: osName, Arch: r.URL.Query().Get("arch")}}
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	var best *api.Release
	for _, rel := range rels {
		if rel.Channel != channel || !rollout.PlatformMatches(rel, dev) {
			continue
		}
		if best == nil || rollout.Compare(rel.Version, best.Version) > 0 {
			best = rel
		}
	}
	if best == nil {
		return nil, store.ErrNotFound
	}
	return best, nil
}
