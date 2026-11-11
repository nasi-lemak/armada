package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/api"
	"github.com/nasi-lemak/armada/internal/attestation"
	"github.com/nasi-lemak/armada/internal/attestation/attestationtest"
	"github.com/nasi-lemak/armada/pkg/license"
)

const agentPkg = "dev.armada.agent"

type androidEnv struct {
	t           *testing.T
	s           *Server
	ts          *httptest.Server
	ca          *attestationtest.CA
	agentSigner []byte
	appSigner   []byte
	token       string
}

func setupAndroid(t *testing.T) *androidEnv {
	t.Helper()
	ca := attestationtest.NewCA(t)
	cfg := DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.AdminTokens = []string{admin}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.Android.ExtraRootsPEM = ca.RootPEM()
	cfg.Android.StatusURL = ""
	cfg.DefaultStrategy = api.Strategy{Stages: []int{100}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	e := &androidEnv{t: t, s: s, ts: ts, ca: ca}
	as := sha256.Sum256([]byte("agent cert"))
	ps := sha256.Sum256([]byte("acme app cert"))
	e.agentSigner, e.appSigner = as[:], ps[:]
	if code := call(t, ts, admin, "PUT", "/api/v1/apps/"+agentPkg, api.App{Name: "Armada agent", Signers: []string{colonHex(as[:])}}, nil); code != 200 {
		t.Fatalf("register agent app: %d", code)
	}
	if code := call(t, ts, admin, "PUT", "/api/v1/apps/com.acme.app", api.App{Name: "Acme", Signers: []string{hex.EncodeToString(ps[:])}, Licensing: true, LicenseTTLSeconds: 600}, nil); code != 200 {
		t.Fatalf("register app: %d", code)
	}
	var tok api.EnrollmentToken
	call(t, ts, admin, "POST", "/api/v1/tokens", api.CreateTokenRequest{Labels: map[string]string{"customer": "acme"}}, &tok)
	e.token = tok.Token
	return e
}

// colonHex formats a digest the way keytool prints it.
func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func (e *androidEnv) challenge() string {
	var c api.ChallengeResponse
	if code := call(e.t, e.ts, "", "POST", "/api/v1/agent/challenge", nil, &c); code != 200 {
		e.t.Fatalf("challenge: %d", code)
	}
	return c.Challenge
}

type fakeDevice struct {
	hw  *ecdsa.PrivateKey
	id  string
	key string
}

// enroll attests a fresh hardware key; mutate adjusts the attestation.
func (e *androidEnv) enroll(mutate func(*attestationtest.Options, *api.EnrollRequest)) (*fakeDevice, int, string) {
	hw, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ch := e.challenge()
	raw, _ := base64.StdEncoding.DecodeString(ch)
	o := attestationtest.Options{Challenge: raw, Package: agentPkg, SignerDigest: e.agentSigner, OSPatchLevel: 202610}
	req := api.EnrollRequest{Token: e.token, Info: api.DeviceInfo{Hostname: "tab", OS: "android", Arch: "arm64", AgentVersion: "1.0.0"}}
	if mutate != nil {
		mutate(&o, &req)
	}
	if req.Attestation == nil && o.Package != "" {
		var chain []string
		for _, c := range e.ca.Attest(&hw.PublicKey, o) {
			chain = append(chain, base64.StdEncoding.EncodeToString(c))
		}
		req.Attestation = &api.AttestationEvidence{Challenge: ch, Chain: chain}
	}
	b, _ := json.Marshal(req)
	resp, err := http.Post(e.ts.URL+"/api/v1/agent/enroll", "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var er api.EnrollResponse
	json.Unmarshal(body, &er)
	return &fakeDevice{hw: hw, id: er.DeviceID, key: er.DeviceKey}, resp.StatusCode, string(body)
}

func (e *androidEnv) requestLicense(d *fakeDevice, appPkg string, signer []byte, appKey *ecdsa.PrivateKey, signWith *ecdsa.PrivateKey, ch string) (api.LicenseResponse, int, string) {
	if ch == "" {
		ch = e.challenge()
	}
	req := api.LicenseRequest{AppPackage: appPkg, AppSigner: hex.EncodeToString(signer), Challenge: ch}
	jkt := ""
	if appKey != nil {
		jwk := license.ECJWK(&appKey.PublicKey)
		req.AppKeyJWK, _ = json.Marshal(jwk)
		jkt = license.Thumbprint(jwk)
	}
	if signWith == nil {
		signWith = d.hw
	}
	digest := sha256.Sum256(api.LicenseSigningInput(d.id, ch, appPkg, req.AppSigner, jkt))
	sig, _ := ecdsa.SignASN1(rand.Reader, signWith, digest[:])
	req.Signature = base64.StdEncoding.EncodeToString(sig)
	b, _ := json.Marshal(req)
	hreq, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/agent/license", bytes.NewReader(b))
	hreq.Header.Set("Authorization", "Bearer "+d.key)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var lr api.LicenseResponse
	json.Unmarshal(body, &lr)
	return lr, resp.StatusCode, string(body)
}

func TestAndroidEnrollmentAttestation(t *testing.T) {
	e := setupAndroid(t)
	d, code, body := e.enroll(nil)
	if code != 200 {
		t.Fatalf("valid enrollment rejected: %d %s", code, body)
	}
	var det struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+d.id, nil, &det)
	a := det.Device.Attestation
	if a == nil || a.SecurityLevel != "tee" || a.BootState != "verified" || !a.DeviceLocked || a.OSPatchLevel != 202610 {
		t.Fatalf("attestation summary: %+v", a)
	}

	cases := []struct {
		name   string
		mutate func(*attestationtest.Options, *api.EnrollRequest)
		want   string
	}{
		{"no attestation", func(o *attestationtest.Options, r *api.EnrollRequest) { o.Package = "" }, "must enroll with a hardware key attestation"},
		{"wrong challenge", func(o *attestationtest.Options, r *api.EnrollRequest) { o.Challenge = []byte("stale") }, "challenge does not match"},
		{"unlocked bootloader", func(o *attestationtest.Options, r *api.EnrollRequest) { o.Unlocked = true }, "boot state"},
		{"custom ROM", func(o *attestationtest.Options, r *api.EnrollRequest) { o.BootState = attestation.BootSelfSigned }, "boot state"},
		{"look-alike app", func(o *attestationtest.Options, r *api.EnrollRequest) { o.Package = "com.evil.agent" }, "allowed app"},
		{"re-signed agent", func(o *attestationtest.Options, r *api.EnrollRequest) { o.SignerDigest = make([]byte, 32) }, "signing certificate"},
		{"unknown challenge", func(o *attestationtest.Options, r *api.EnrollRequest) {
			r.Attestation = &api.AttestationEvidence{Challenge: base64.StdEncoding.EncodeToString([]byte("made up")), Chain: []string{"AA=="}}
		}, "unknown or expired challenge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, code, body := e.enroll(c.mutate)
			if code != 403 || !strings.Contains(body, c.want) {
				t.Fatalf("got %d %s, want 403 mentioning %q", code, body, c.want)
			}
		})
	}
	// Challenges are single use.
	ch := e.challenge()
	if !e.s.challenges.consume(ch, time.Now()) || e.s.challenges.consume(ch, time.Now()) {
		t.Fatal("challenge reusable")
	}
}

func TestAndroidLicensing(t *testing.T) {
	e := setupAndroid(t)
	d, code, body := e.enroll(nil)
	if code != 200 {
		t.Fatalf("enroll: %d %s", code, body)
	}
	appKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	lr, code, body := e.requestLicense(d, "com.acme.app", e.appSigner, appKey, nil, "")
	if code != 200 {
		t.Fatalf("license: %d %s", code, body)
	}
	// The customer's backend verifies it with the public JWKS, using pkg/license.
	resp, err := http.Get(e.ts.URL + "/api/v1/license/jwks")
	if err != nil {
		t.Fatal(err)
	}
	jwks, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	v, err := license.NewVerifierFromJWKS(jwks, "com.acme.app")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := v.Verify(lr.License, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claims.DeviceID() != d.id || claims.Device.Labels["customer"] != "acme" || claims.Confirmation == nil {
		t.Fatalf("claims: %+v", claims)
	}
	if ttl := time.Until(lr.ExpiresAt); ttl > 10*time.Minute || ttl < 9*time.Minute {
		t.Fatalf("license ttl %s, want the app's 600s", ttl)
	}
	proof, _ := license.MakeDPoPProof(appKey, "GET", "https://api.acme.com/me", lr.License, time.Now())
	if err := v.VerifyDPoP(claims, lr.License, proof, "GET", "https://api.acme.com/me", time.Now()); err != nil {
		t.Fatalf("DPoP: %v", err)
	}

	// Rejections.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, code, body := e.requestLicense(d, "com.acme.app", e.appSigner, nil, other, ""); code != 403 || !strings.Contains(body, "hardware key signature") {
		t.Fatalf("signature from a non-hardware key: %d %s", code, body)
	}
	if _, code, body := e.requestLicense(d, "com.acme.app", make([]byte, 32), nil, nil, ""); code != 403 || !strings.Contains(body, "signing certificate") {
		t.Fatalf("re-signed (pirated) app: %d %s", code, body)
	}
	if _, code, _ := e.requestLicense(d, "com.other.app", e.appSigner, nil, nil, ""); code != 403 {
		t.Fatalf("unregistered app: %d", code)
	}
	if _, code, _ := e.requestLicense(d, agentPkg, e.agentSigner, nil, nil, ""); code != 403 {
		t.Fatalf("app without licensing enabled: %d", code)
	}
	ch := e.challenge()
	if _, code, _ := e.requestLicense(d, "com.acme.app", e.appSigner, nil, nil, ch); code != 200 {
		t.Fatalf("first use of challenge: %d", code)
	}
	if _, code, body := e.requestLicense(d, "com.acme.app", e.appSigner, nil, nil, ch); code != 403 || !strings.Contains(body, "challenge") {
		t.Fatalf("replayed challenge: %d %s", code, body)
	}

	// Revoking the device stops new licenses; reinstating restores them.
	tr, fa := true, false
	call(t, e.ts, admin, "PATCH", "/api/v1/devices/"+d.id, map[string]any{"license_revoked": tr}, nil)
	if _, code, body := e.requestLicense(d, "com.acme.app", e.appSigner, nil, nil, ""); code != 403 || !strings.Contains(body, "revoked") {
		t.Fatalf("revoked device: %d %s", code, body)
	}
	call(t, e.ts, admin, "PATCH", "/api/v1/devices/"+d.id, map[string]any{"license_revoked": fa}, nil)
	if _, code, _ := e.requestLicense(d, "com.acme.app", e.appSigner, nil, nil, ""); code != 200 {
		t.Fatalf("reinstated device: %d", code)
	}

	// A Linux device (no attested key) cannot get Android licenses.
	var er api.EnrollResponse
	call(t, e.ts, "", "POST", "/api/v1/agent/enroll", api.EnrollRequest{Token: e.token, Info: api.DeviceInfo{OS: "linux"}}, &er)
	if _, code, body := e.requestLicense(&fakeDevice{hw: other, id: er.DeviceID, key: er.DeviceKey}, "com.acme.app", e.appSigner, nil, nil, ""); code != 403 || !strings.Contains(body, "no attested hardware key") {
		t.Fatalf("device without attested key: %d %s", code, body)
	}
}

func uploadAPK(t *testing.T, ts *httptest.Server, pkg, version, versionCode string) api.Release {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("package", pkg)
	mw.WriteField("version", version)
	mw.WriteField("os", "android")
	mw.WriteField("version_code", versionCode)
	fw, _ := mw.CreateFormFile("file", "app.apk")
	fw.Write([]byte("PK fake apk " + version))
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/releases", &buf)
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload: %d %s", resp.StatusCode, b)
	}
	var res uploadResult
	json.NewDecoder(resp.Body).Decode(&res)
	return *res.Release
}

func TestAndroidUpdatesAndPolicy(t *testing.T) {
	e := setupAndroid(t)
	d, code, _ := e.enroll(nil)
	if code != 200 {
		t.Fatal("enroll failed")
	}
	rel := uploadAPK(t, e.ts, "com.acme.app", "2.1.0", "210")
	call(t, e.ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: rel.ID}, nil)
	var hr api.HeartbeatResponse
	call(t, e.ts, d.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{
		Info: api.DeviceInfo{OS: "android", AgentVersion: "1.0.0"}, Packages: map[string]string{"com.acme.app": "2.0.0"},
	}, &hr)
	if hr.Update == nil || hr.Update.VersionCode != 210 || len(hr.Update.Signers) != 1 || hr.Update.Signers[0] != hex.EncodeToString(e.appSigner) {
		t.Fatalf("update instruction: %+v", hr.Update)
	}
	if hr.Policy == nil || !hr.Policy.DisableDebugging || !hr.Policy.BlockUninstall {
		t.Fatalf("policy: %+v", hr.Policy)
	}
	if strings.Join(hr.ManagedPackages, ",") != "com.acme.app,"+agentPkg {
		t.Fatalf("managed packages: %v", hr.ManagedPackages)
	}
	// The Android agent's version is tracked under its own package, never the Linux agent's.
	var det struct{ Device api.Device }
	call(t, e.ts, admin, "GET", "/api/v1/devices/"+d.id, nil, &det)
	if det.Device.Packages[agentPkg] != "1.0.0" || det.Device.Packages[api.AgentPackage] != "" {
		t.Fatalf("packages: %v", det.Device.Packages)
	}
	linuxAgent := upload(t, e.ts, api.AgentPackage, "9.9.9")
	call(t, e.ts, admin, "POST", "/api/v1/deployments", api.CreateDeploymentRequest{ReleaseID: linuxAgent.ID}, nil)
	hr = api.HeartbeatResponse{}
	call(t, e.ts, d.key, "POST", "/api/v1/agent/heartbeat", api.HeartbeatRequest{
		Info: api.DeviceInfo{OS: "android", AgentVersion: "1.0.0"}, Packages: map[string]string{"com.acme.app": "2.1.0"},
	}, &hr)
	if hr.Update != nil && hr.Update.Package == api.AgentPackage {
		t.Fatalf("Linux agent offered to an Android device: %+v", hr.Update)
	}
}

func TestAndroidProvisioningQR(t *testing.T) {
	e := setupAndroid(t)
	var pr provisioningResponse
	if code := call(t, e.ts, admin, "POST", "/api/v1/android/provisioning", provisioningRequest{EnrollmentToken: e.token}, &pr); code != 409 {
		t.Fatalf("provisioning without an agent release: %d", code)
	}
	agentRel := uploadAPK(t, e.ts, agentPkg, "1.0.0", "1")
	if code := call(t, e.ts, admin, "POST", "/api/v1/android/provisioning", provisioningRequest{EnrollmentToken: e.token, WifiSSID: "factory"}, &pr); code != 200 {
		t.Fatalf("provisioning: %d", code)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(pr.Payload), &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_SIGNATURE_CHECKSUM"]; got != base64.RawURLEncoding.EncodeToString(e.agentSigner) {
		t.Fatalf("checksum %v", got)
	}
	if got := payload["android.app.extra.PROVISIONING_DEVICE_ADMIN_COMPONENT_NAME"]; got != agentPkg+"/"+agentPkg+".AdminReceiver" {
		t.Fatalf("component %v", got)
	}
	extras := payload["android.app.extra.PROVISIONING_ADMIN_EXTRAS_BUNDLE"].(map[string]any)
	if extras["enrollment_token"] != e.token || extras["server_url"] != e.ts.URL {
		t.Fatalf("extras %v", extras)
	}
	if payload["android.app.extra.PROVISIONING_WIFI_SSID"] != "factory" || payload["android.app.extra.PROVISIONING_LEAVE_ALL_SYSTEM_APPS_ENABLED"] != true {
		t.Fatalf("payload %v", payload)
	}
	png, _ := base64.StdEncoding.DecodeString(pr.QRPNG)
	if !bytes.HasPrefix(png, []byte("\x89PNG")) || len(pr.Warnings) == 0 {
		t.Fatalf("qr png %d bytes, warnings %v", len(png), pr.Warnings)
	}
	// The download URL in the QR works without credentials and serves the agent APK.
	resp, err := http.Get(pr.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Armada-SHA256") != agentRel.SHA256 || !bytes.HasPrefix(body, []byte("PK fake apk")) {
		t.Fatalf("agent download: %d %s", resp.StatusCode, body)
	}
	// Customer APKs are never public.
	uploadAPK(t, e.ts, "com.acme.app", "1.0.0", "100")
	resp, _ = http.Get(e.ts.URL + "/api/v1/agent/download?package=com.acme.app&os=android")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("public download of customer app: %d", resp.StatusCode)
	}
}
