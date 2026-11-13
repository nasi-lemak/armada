// Package certs holds the certificate handling shared by the agent (which
// keeps the keys) and the server (which tracks and matches certificates).
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/nasi-lemak/armada/internal/api"
)

// KeyTypes a CSR can ask for. ECDSA P-256 is the default; RSA is there for
// clients and CAs that need it.
var KeyTypes = []string{"ecdsa-p256", "ecdsa-p384", "rsa-2048", "rsa-3072", "rsa-4096"}

// ValidKeyType reports whether t is one of KeyTypes.
func ValidKeyType(t string) bool {
	for _, k := range KeyTypes {
		if k == t {
			return true
		}
	}
	return false
}

// KeyTypeOf names a public key's type, e.g. "rsa-2048".
func KeyTypeOf(pub any) string {
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return fmt.Sprintf("rsa-%d", k.N.BitLen())
	case *ecdsa.PublicKey:
		return "ecdsa-" + strings.ToLower(strings.ReplaceAll(k.Curve.Params().Name, "-", ""))
	case ed25519.PublicKey:
		return "ed25519"
	}
	return "unknown"
}

// PublicKeyID is the SHA-256 of a public key's PKIX encoding (hex): the
// same for a key, its CSR and every certificate issued for it.
func PublicKeyID(pub any) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Describe summarises a certificate (KeyMatches is left for the caller).
func Describe(c *x509.Certificate) api.CertInfo {
	sum := sha256.Sum256(c.Raw)
	info := api.CertInfo{
		Subject: c.Subject.String(), CommonName: c.Subject.CommonName, Fields: SubjectOf(c.Subject), DNSNames: c.DNSNames,
		Issuer: c.Issuer.String(), Serial: strings.ToUpper(c.SerialNumber.Text(16)),
		NotBefore: c.NotBefore.UTC(), NotAfter: c.NotAfter.UTC(), KeyType: KeyTypeOf(c.PublicKey),
		Fingerprint: hex.EncodeToString(sum[:]),
	}
	for _, ip := range c.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	return info
}

// ParseCerts returns every certificate in PEM data, in order.
func ParseCerts(data []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// EncodeCerts writes certificates as PEM.
func EncodeCerts(cs ...*x509.Certificate) []byte {
	var out []byte
	for _, c := range cs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// ParseCSR reads a PEM certificate request and checks its signature.
func ParseCSR(data []byte) (*x509.CertificateRequest, error) {
	b, _ := pem.Decode(data)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	return csr, nil
}

// GenerateKey makes a private key of a KeyTypes type.
func GenerateKey(keyType string) (crypto.Signer, error) {
	switch keyType {
	case "", "ecdsa-p256":
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "ecdsa-p384":
		return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	case "rsa-2048":
		return rsa.GenerateKey(rand.Reader, 2048)
	case "rsa-3072":
		return rsa.GenerateKey(rand.Reader, 3072)
	case "rsa-4096":
		return rsa.GenerateKey(rand.Reader, 4096)
	}
	return nil, fmt.Errorf("unsupported key type %q", keyType)
}

// EncodeKey writes a private key as PEM (PKCS#8, which OpenSSL, Mosquitto
// and most servers read).
func EncodeKey(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKey reads a PEM private key (PKCS#8, PKCS#1 or SEC 1).
func ParseKey(data []byte) (crypto.Signer, error) {
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			return nil, errors.New("no private key found")
		}
		switch b.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
			if err != nil {
				return nil, err
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("unsupported private key")
			}
			return s, nil
		case "RSA PRIVATE KEY":
			return x509.ParsePKCS1PrivateKey(b.Bytes)
		case "EC PRIVATE KEY":
			return x509.ParseECPrivateKey(b.Bytes)
		}
	}
}

// MakeCSR builds a PEM certificate request signed by key.
func MakeCSR(key crypto.Signer, subj api.CertSubject, dnsNames, ips []string) ([]byte, error) {
	tpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: subj.CommonName}, DNSNames: dnsNames}
	set := func(dst *[]string, v string) {
		if v != "" {
			*dst = []string{v}
		}
	}
	set(&tpl.Subject.Organization, subj.Organization)
	set(&tpl.Subject.OrganizationalUnit, subj.OrganizationalUnit)
	set(&tpl.Subject.Country, subj.Country)
	set(&tpl.Subject.Province, subj.Province)
	set(&tpl.Subject.Locality, subj.Locality)
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address %q", s)
		}
		tpl.IPAddresses = append(tpl.IPAddresses, ip)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// SubjectOf extracts the CSR-relevant parts of a certificate's subject.
func SubjectOf(n pkix.Name) api.CertSubject {
	first := func(v []string) string {
		if len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return api.CertSubject{CommonName: n.CommonName, Organization: first(n.Organization), OrganizationalUnit: first(n.OrganizationalUnit),
		Country: first(n.Country), Province: first(n.Province), Locality: first(n.Locality)}
}
