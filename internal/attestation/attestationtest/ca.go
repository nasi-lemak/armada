// Package attestationtest mints Android-style key attestation chains from a
// throwaway CA, for tests. Production code trusts only Google's roots, so
// these chains are accepted only when the CA's root is added explicitly.
package attestationtest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/nasi-lemak/armada/internal/attestation"
)

// CA is a fake attestation root and intermediate.
type CA struct {
	t         testing.TB
	rootKey   *ecdsa.PrivateKey
	root      *x509.Certificate
	interKey  *ecdsa.PrivateKey
	inter     *x509.Certificate
	interDER  []byte
	rootDER   []byte
	serialSeq int64
}

// NewCA creates a root and an intermediate.
func NewCA(t testing.TB) *CA {
	t.Helper()
	ca := &CA{t: t, serialSeq: 100}
	ca.rootKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca.interKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Attestation Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	var err error
	ca.rootDER, err = x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &ca.rootKey.PublicKey, ca.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	ca.root, _ = x509.ParseCertificate(ca.rootDER)
	interTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Test Attestation Intermediate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	ca.interDER, err = x509.CreateCertificate(rand.Reader, interTmpl, ca.root, &ca.interKey.PublicKey, ca.rootKey)
	if err != nil {
		t.Fatal(err)
	}
	ca.inter, _ = x509.ParseCertificate(ca.interDER)
	return ca
}

// RootPEM is the root certificate to trust.
func (ca *CA) RootPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.rootDER})
}

// Options describe the attested key.
type Options struct {
	Challenge     []byte
	SecurityLevel attestation.SecurityLevel // default TEE
	BootState     attestation.BootState
	Unlocked      bool
	Package       string
	SignerDigest  []byte // SHA-256 of the app signing certificate
	OSPatchLevel  int64
}

// Attest returns a DER chain (leaf first) attesting pub.
func (ca *CA) Attest(pub crypto.PublicKey, o Options) [][]byte {
	ca.t.Helper()
	if o.SecurityLevel == attestation.Software {
		o.SecurityLevel = attestation.TrustedEnvironment
	}
	ca.serialSeq++
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serialSeq), Subject: pkix.Name{CommonName: "Android Keystore Key"},
		NotBefore: time.Unix(0, 0), NotAfter: time.Date(2048, 1, 1, 0, 0, 0, 0, time.UTC),
		ExtraExtensions: []pkix.Extension{{Id: attestation.OIDKeyDescription, Value: KeyDescription(o)}},
	}
	leaf, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca.inter, pub, ca.interKey)
	if err != nil {
		ca.t.Fatal(err)
	}
	return [][]byte{leaf, ca.interDER, ca.rootDER}
}

// KeyDescription encodes an attestation record (attestation version 200, KeyMint).
func KeyDescription(o Options) []byte {
	appID := seq(
		set(seq(octets([]byte(o.Package)), integer(1))),
		set(octets(o.SignerDigest)),
	)
	sw := seq(explicit(709, octets(appID)))
	rot := seq(octets(make([]byte, 32)), boolean(!o.Unlocked), enumerated(int64(o.BootState)), octets(make([]byte, 32)))
	hw := seq(explicit(704, rot), explicit(705, integer(150000)), explicit(706, integer(o.OSPatchLevel)))
	lvl := int64(o.SecurityLevel)
	return seq(integer(200), enumerated(lvl), integer(200), enumerated(lvl), octets(o.Challenge), octets(nil), sw, hw)
}

// ---- tiny DER writer ----

func tlv(class int, constructed bool, tag int, content []byte) []byte {
	id := byte(class << 6)
	if constructed {
		id |= 0x20
	}
	var out []byte
	if tag < 31 {
		out = append(out, id|byte(tag))
	} else {
		out = append(out, id|0x1f)
		var t []byte
		for v := tag; v > 0; v >>= 7 {
			t = append([]byte{byte(v & 0x7f)}, t...)
		}
		for i := 0; i < len(t)-1; i++ {
			t[i] |= 0x80
		}
		out = append(out, t...)
	}
	n := len(content)
	switch {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	default:
		out = append(out, 0x82, byte(n>>8), byte(n))
	}
	return append(out, content...)
}

func concat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func seq(parts ...[]byte) []byte { return tlv(0, true, 16, concat(parts...)) }
func set(parts ...[]byte) []byte { return tlv(0, true, 17, concat(parts...)) }
func octets(b []byte) []byte     { return tlv(0, false, 4, b) }
func explicit(tag int, inner []byte) []byte {
	return tlv(2, true, tag, inner)
}
func boolean(v bool) []byte {
	if v {
		return tlv(0, false, 1, []byte{0xff})
	}
	return tlv(0, false, 1, []byte{0})
}
func integer(v int64) []byte    { return tlv(0, false, 2, intBytes(v)) }
func enumerated(v int64) []byte { return tlv(0, false, 10, intBytes(v)) }

func intBytes(v int64) []byte {
	b := big.NewInt(v).Bytes()
	if len(b) == 0 {
		return []byte{0}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return b
}
