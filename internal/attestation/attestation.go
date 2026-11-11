// Package attestation verifies Android hardware key attestation.
//
// When an Android device generates a key in its secure hardware (TEE or
// StrongBox) with an attestation challenge, the Keystore returns a certificate
// chain. The leaf certificate holds the new public key plus a KeyDescription
// extension describing where the key lives, the challenge, the device's boot
// state and which app created it. The chain ends at a Google root.
//
// Verifying the chain proves that the private key is held by real Android
// secure hardware and cannot be exported, which is what lets Armada bind a
// license to one physical device.
//
// Verification follows Google's guidance
// (https://developer.android.com/privacy-and-security/security-key-attestation):
// check each signature up the chain, check the root's public key against
// Google's published roots, check the revocation list, and check the
// extension appears only in the leaf. Certificate validity dates are not
// enforced, because attestation leaf certificates routinely carry
// placeholder dates.
package attestation

import (
	"bytes"
	"crypto"
	"crypto/subtle"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

//go:embed google_roots.pem
var googleRootsPEM []byte

// OIDKeyDescription is the X.509 extension carrying the attestation record.
var OIDKeyDescription = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 1, 17}

// SecurityLevel says where a key lives.
type SecurityLevel int

// Security levels, as encoded in the attestation record.
const (
	Software           SecurityLevel = 0
	TrustedEnvironment SecurityLevel = 1
	StrongBox          SecurityLevel = 2
)

func (s SecurityLevel) String() string {
	switch s {
	case Software:
		return "software"
	case TrustedEnvironment:
		return "tee"
	case StrongBox:
		return "strongbox"
	}
	return fmt.Sprintf("unknown(%d)", int(s))
}

// rank orders security levels; StrongBox is stronger than a TEE.
func (s SecurityLevel) rank() int {
	switch s {
	case TrustedEnvironment:
		return 1
	case StrongBox:
		return 2
	}
	return 0
}

// BootState is the device's verified boot state.
type BootState int

// Verified boot states.
const (
	BootVerified   BootState = 0 // locked bootloader, Google/OEM-signed OS
	BootSelfSigned BootState = 1 // locked bootloader, custom signing key
	BootUnverified BootState = 2 // unlocked bootloader
	BootFailed     BootState = 3
)

func (b BootState) String() string {
	switch b {
	case BootVerified:
		return "verified"
	case BootSelfSigned:
		return "self-signed"
	case BootUnverified:
		return "unverified"
	case BootFailed:
		return "failed"
	}
	return fmt.Sprintf("unknown(%d)", int(b))
}

// RootOfTrust describes the device's boot state.
type RootOfTrust struct {
	VerifiedBootKey   []byte
	DeviceLocked      bool
	VerifiedBootState BootState
	VerifiedBootHash  []byte
}

// PackageInfo names an app that was allowed to use the key.
type PackageInfo struct {
	Name    string
	Version int64
}

// ApplicationID identifies the app that generated the key.
type ApplicationID struct {
	Packages []PackageInfo
	// SignatureDigests are SHA-256 digests of the app's signing certificates.
	SignatureDigests [][]byte
}

// AuthorizationList is the subset of key properties Armada uses.
type AuthorizationList struct {
	RootOfTrust   *RootOfTrust
	ApplicationID *ApplicationID
	OSVersion     int64
	OSPatchLevel  int64
}

// KeyDescription is the parsed attestation extension.
type KeyDescription struct {
	AttestationVersion       int64
	AttestationSecurityLevel SecurityLevel
	KeymasterVersion         int64
	KeymasterSecurityLevel   SecurityLevel
	Challenge                []byte
	UniqueID                 []byte
	SoftwareEnforced         AuthorizationList
	HardwareEnforced         AuthorizationList
}

// Verifier checks attestation chains against trusted roots.
type Verifier struct {
	roots   [][]byte // DER-encoded SubjectPublicKeyInfo of each trusted root
	revoked func(serialHex string) (bool, string)
}

// NewVerifier trusts Google's published attestation roots plus any extra
// PEM-encoded root certificates (for testing or private PKI). revoked may be
// nil; otherwise it reports whether a certificate serial (lowercase hex) is
// revoked, and why.
func NewVerifier(extraRootsPEM []byte, revoked func(serialHex string) (bool, string)) (*Verifier, error) {
	v := &Verifier{revoked: revoked}
	for _, src := range [][]byte{googleRootsPEM, extraRootsPEM} {
		rest := src
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("parse root: %w", err)
			}
			v.addRoot(cert.RawSubjectPublicKeyInfo)
		}
	}
	if len(v.roots) == 0 {
		return nil, errors.New("no trusted roots")
	}
	return v, nil
}

func (v *Verifier) addRoot(spki []byte) {
	for _, r := range v.roots {
		if bytes.Equal(r, spki) {
			return
		}
	}
	v.roots = append(v.roots, spki)
}

// Policy states what an acceptable attestation looks like.
type Policy struct {
	// Challenge must match the attestation challenge exactly. Required.
	Challenge []byte
	// MinSecurityLevel is the weakest acceptable key storage (default TEE).
	MinSecurityLevel SecurityLevel
	// RequireVerifiedBoot rejects devices with an unlocked bootloader or a
	// modified OS (boot state must be verified and the device locked).
	RequireVerifiedBoot bool
	// AllowedPackages, if set, requires the key to belong to one of these apps.
	AllowedPackages []string
	// AllowedSigners, if set, requires the app's signing certificate SHA-256
	// (hex) to be one of these.
	AllowedSigners []string
}

// Result is a successfully verified attestation.
type Result struct {
	// PublicKey is the attested hardware key (*ecdsa.PublicKey or *rsa.PublicKey).
	PublicKey      crypto.PublicKey
	PublicKeyDER   []byte // PKIX encoding of PublicKey
	SecurityLevel  SecurityLevel
	RootOfTrust    *RootOfTrust
	ApplicationID  *ApplicationID
	OSVersion      int64
	OSPatchLevel   int64
	KeyDescription *KeyDescription
}

// Verify checks a DER certificate chain (leaf first) against the policy.
func (v *Verifier) Verify(chain [][]byte, p Policy) (*Result, error) {
	if len(chain) < 2 {
		return nil, errors.New("attestation chain too short")
	}
	if len(chain) > 10 {
		return nil, errors.New("attestation chain too long")
	}
	if len(p.Challenge) == 0 {
		return nil, errors.New("policy has no challenge")
	}
	certs := make([]*x509.Certificate, len(chain))
	for i, der := range chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", i, err)
		}
		certs[i] = c
	}
	// Each certificate must be signed by the next one; the last is self-signed.
	for i := 0; i < len(certs); i++ {
		parent := certs[len(certs)-1]
		if i < len(certs)-1 {
			parent = certs[i+1]
		}
		if err := parent.CheckSignature(certs[i].SignatureAlgorithm, certs[i].RawTBSCertificate, certs[i].Signature); err != nil {
			return nil, fmt.Errorf("certificate %d is not signed by certificate %d: %w", i, min(i+1, len(certs)-1), err)
		}
	}
	root := certs[len(certs)-1].RawSubjectPublicKeyInfo
	trusted := false
	for _, r := range v.roots {
		if bytes.Equal(r, root) {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, errors.New("chain does not end at a trusted Google attestation root")
	}
	if v.revoked != nil {
		for i, c := range certs {
			if bad, why := v.revoked(strings.ToLower(c.SerialNumber.Text(16))); bad {
				return nil, fmt.Errorf("certificate %d (serial %s) is revoked: %s", i, c.SerialNumber.Text(16), why)
			}
		}
	}
	var ext []byte
	for i, c := range certs {
		for _, e := range c.Extensions {
			if e.Id.Equal(OIDKeyDescription) {
				if i != 0 {
					return nil, errors.New("attestation extension found outside the leaf certificate")
				}
				ext = e.Value
			}
		}
	}
	if ext == nil {
		return nil, errors.New("leaf certificate has no attestation extension")
	}
	kd, err := ParseKeyDescription(ext)
	if err != nil {
		return nil, fmt.Errorf("parse attestation record: %w", err)
	}

	if subtle.ConstantTimeCompare(kd.Challenge, p.Challenge) != 1 {
		return nil, errors.New("attestation challenge does not match")
	}
	minLevel := p.MinSecurityLevel
	if minLevel == Software {
		minLevel = TrustedEnvironment
	}
	if kd.AttestationSecurityLevel.rank() < minLevel.rank() || kd.KeymasterSecurityLevel.rank() < minLevel.rank() {
		return nil, fmt.Errorf("key is stored in %s, need at least %s", kd.AttestationSecurityLevel, minLevel)
	}
	rot := kd.HardwareEnforced.RootOfTrust
	if rot == nil {
		return nil, errors.New("attestation has no hardware root of trust")
	}
	if p.RequireVerifiedBoot && (rot.VerifiedBootState != BootVerified || !rot.DeviceLocked) {
		return nil, fmt.Errorf("device boot state is %s (locked=%v); a locked bootloader with a verified OS is required", rot.VerifiedBootState, rot.DeviceLocked)
	}
	appID := kd.SoftwareEnforced.ApplicationID
	if appID == nil {
		appID = kd.HardwareEnforced.ApplicationID
	}
	if len(p.AllowedPackages) > 0 {
		if appID == nil || !anyPackage(appID.Packages, p.AllowedPackages) {
			return nil, errors.New("attested key was not created by an allowed app")
		}
	}
	if len(p.AllowedSigners) > 0 {
		if appID == nil || !anySigner(appID.SignatureDigests, p.AllowedSigners) {
			return nil, errors.New("attested key was not created by an app with an allowed signing certificate")
		}
	}
	leaf := certs[0]
	osv, osp := kd.HardwareEnforced.OSVersion, kd.HardwareEnforced.OSPatchLevel
	return &Result{
		PublicKey:      leaf.PublicKey,
		PublicKeyDER:   leaf.RawSubjectPublicKeyInfo,
		SecurityLevel:  kd.AttestationSecurityLevel,
		RootOfTrust:    rot,
		ApplicationID:  appID,
		OSVersion:      osv,
		OSPatchLevel:   osp,
		KeyDescription: kd,
	}, nil
}

func anyPackage(have []PackageInfo, want []string) bool {
	for _, h := range have {
		for _, w := range want {
			if h.Name == w {
				return true
			}
		}
	}
	return false
}

func anySigner(have [][]byte, want []string) bool {
	for _, h := range have {
		hx := hex.EncodeToString(h)
		for _, w := range want {
			if strings.EqualFold(normalizeDigest(w), hx) {
				return true
			}
		}
	}
	return false
}

// normalizeDigest accepts "AB:CD:..." (keytool style) or plain hex.
func normalizeDigest(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
}

// NormalizeDigest is exported for callers storing signer digests.
func NormalizeDigest(s string) string { return normalizeDigest(s) }

// ---- KeyDescription parsing ----

// Authorization list tags used by Armada (see the KeyMint AIDL docs).
const (
	tagRootOfTrust   = 704
	tagOSVersion     = 705
	tagOSPatchLevel  = 706
	tagApplicationID = 709
)

// ParseKeyDescription parses the DER contents of the attestation extension.
func ParseKeyDescription(der []byte) (*KeyDescription, error) {
	seq, rest, err := readTLV(der)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 || !seq.isUniversal(tagSequence) {
		return nil, errors.New("KeyDescription is not a single SEQUENCE")
	}
	f, err := children(seq.content)
	if err != nil {
		return nil, err
	}
	if len(f) < 8 {
		return nil, fmt.Errorf("KeyDescription has %d fields, want 8", len(f))
	}
	var kd KeyDescription
	var lvl int64
	if kd.AttestationVersion, err = f[0].integer(); err != nil {
		return nil, fmt.Errorf("attestationVersion: %w", err)
	}
	if lvl, err = f[1].enumerated(); err != nil {
		return nil, fmt.Errorf("attestationSecurityLevel: %w", err)
	}
	kd.AttestationSecurityLevel = SecurityLevel(lvl)
	if kd.KeymasterVersion, err = f[2].integer(); err != nil {
		return nil, fmt.Errorf("keymasterVersion: %w", err)
	}
	if lvl, err = f[3].enumerated(); err != nil {
		return nil, fmt.Errorf("keymasterSecurityLevel: %w", err)
	}
	kd.KeymasterSecurityLevel = SecurityLevel(lvl)
	if kd.Challenge, err = f[4].octets(); err != nil {
		return nil, fmt.Errorf("attestationChallenge: %w", err)
	}
	if kd.UniqueID, err = f[5].octets(); err != nil {
		return nil, fmt.Errorf("uniqueId: %w", err)
	}
	if kd.SoftwareEnforced, err = parseAuthList(f[6]); err != nil {
		return nil, fmt.Errorf("softwareEnforced: %w", err)
	}
	if kd.HardwareEnforced, err = parseAuthList(f[7]); err != nil {
		return nil, fmt.Errorf("hardwareEnforced: %w", err)
	}
	return &kd, nil
}

func parseAuthList(t tlv) (AuthorizationList, error) {
	var al AuthorizationList
	if !t.isUniversal(tagSequence) {
		return al, errors.New("not a SEQUENCE")
	}
	items, err := children(t.content)
	if err != nil {
		return al, err
	}
	for _, it := range items {
		if it.class != classContext || !it.constructed {
			continue
		}
		inner, rest, err := readTLV(it.content)
		if err != nil || len(rest) != 0 {
			return al, fmt.Errorf("tag %d: malformed explicit value", it.tag)
		}
		switch it.tag {
		case tagRootOfTrust:
			rot, err := parseRootOfTrust(inner)
			if err != nil {
				return al, fmt.Errorf("rootOfTrust: %w", err)
			}
			al.RootOfTrust = rot
		case tagApplicationID:
			raw, err := inner.octets()
			if err != nil {
				return al, fmt.Errorf("attestationApplicationId: %w", err)
			}
			app, err := parseApplicationID(raw)
			if err != nil {
				return al, fmt.Errorf("attestationApplicationId: %w", err)
			}
			al.ApplicationID = app
		case tagOSVersion:
			al.OSVersion, _ = inner.integer()
		case tagOSPatchLevel:
			al.OSPatchLevel, _ = inner.integer()
		}
	}
	return al, nil
}

func parseRootOfTrust(t tlv) (*RootOfTrust, error) {
	if !t.isUniversal(tagSequence) {
		return nil, errors.New("not a SEQUENCE")
	}
	f, err := children(t.content)
	if err != nil {
		return nil, err
	}
	if len(f) < 3 {
		return nil, errors.New("too few fields")
	}
	var r RootOfTrust
	if r.VerifiedBootKey, err = f[0].octets(); err != nil {
		return nil, err
	}
	if r.DeviceLocked, err = f[1].boolean(); err != nil {
		return nil, err
	}
	state, err := f[2].enumerated()
	if err != nil {
		return nil, err
	}
	r.VerifiedBootState = BootState(state)
	if len(f) > 3 {
		r.VerifiedBootHash, _ = f[3].octets()
	}
	return &r, nil
}

func parseApplicationID(der []byte) (*ApplicationID, error) {
	seq, _, err := readTLV(der)
	if err != nil {
		return nil, err
	}
	f, err := children(seq.content)
	if err != nil {
		return nil, err
	}
	if len(f) < 2 {
		return nil, errors.New("too few fields")
	}
	var app ApplicationID
	pkgs, err := children(f[0].content)
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		pf, err := children(p.content)
		if err != nil || len(pf) < 2 {
			return nil, errors.New("malformed package info")
		}
		name, err := pf[0].octets()
		if err != nil {
			return nil, err
		}
		ver, err := pf[1].integer()
		if err != nil {
			return nil, err
		}
		app.Packages = append(app.Packages, PackageInfo{Name: string(name), Version: ver})
	}
	digests, err := children(f[1].content)
	if err != nil {
		return nil, err
	}
	for _, d := range digests {
		b, err := d.octets()
		if err != nil {
			return nil, err
		}
		app.SignatureDigests = append(app.SignatureDigests, b)
	}
	return &app, nil
}

// ---- minimal DER reader (supports high tag numbers, which encoding/asn1 struct parsing can't express for 700+ optional tags) ----

const (
	classUniversal = 0
	classContext   = 2

	tagBoolean    = 1
	tagInteger    = 2
	tagOctets     = 4
	tagEnumerated = 10
	tagSequence   = 16
)

type tlv struct {
	class       int
	constructed bool
	tag         int
	content     []byte
}

func (t tlv) isUniversal(tag int) bool { return t.class == classUniversal && t.tag == tag }

func readTLV(b []byte) (tlv, []byte, error) {
	var t tlv
	if len(b) < 2 {
		return t, nil, errors.New("truncated DER")
	}
	id := b[0]
	t.class = int(id >> 6)
	t.constructed = id&0x20 != 0
	t.tag = int(id & 0x1f)
	i := 1
	if t.tag == 0x1f {
		t.tag = 0
		for {
			if i >= len(b) || i > 5 {
				return t, nil, errors.New("bad high tag number")
			}
			c := b[i]
			i++
			t.tag = t.tag<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
	}
	if i >= len(b) {
		return t, nil, errors.New("truncated DER length")
	}
	l := int(b[i])
	i++
	if l&0x80 != 0 {
		n := l & 0x7f
		if n == 0 || n > 4 || i+n > len(b) {
			return t, nil, errors.New("bad DER length")
		}
		l = 0
		for _, c := range b[i : i+n] {
			l = l<<8 | int(c)
		}
		i += n
	}
	if l < 0 || i+l > len(b) {
		return t, nil, errors.New("DER value overruns input")
	}
	t.content = b[i : i+l]
	return t, b[i+l:], nil
}

func children(b []byte) ([]tlv, error) {
	var out []tlv
	for len(b) > 0 {
		t, rest, err := readTLV(b)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		b = rest
	}
	return out, nil
}

func (t tlv) intValue(tag int) (int64, error) {
	if !t.isUniversal(tag) {
		return 0, fmt.Errorf("expected universal tag %d, got class %d tag %d", tag, t.class, t.tag)
	}
	if len(t.content) == 0 || len(t.content) > 8 {
		return 0, errors.New("integer out of range")
	}
	v := int64(int8(t.content[0])) // sign-extend
	for _, c := range t.content[1:] {
		v = v<<8 | int64(c)
	}
	return v, nil
}

func (t tlv) integer() (int64, error)    { return t.intValue(tagInteger) }
func (t tlv) enumerated() (int64, error) { return t.intValue(tagEnumerated) }

func (t tlv) octets() ([]byte, error) {
	if !t.isUniversal(tagOctets) {
		return nil, fmt.Errorf("expected OCTET STRING, got class %d tag %d", t.class, t.tag)
	}
	return t.content, nil
}

func (t tlv) boolean() (bool, error) {
	if !t.isUniversal(tagBoolean) || len(t.content) != 1 {
		return false, errors.New("expected BOOLEAN")
	}
	return t.content[0] != 0, nil
}
