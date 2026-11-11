package attestation_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/nasi-lemak/armada/internal/attestation"
	"github.com/nasi-lemak/armada/internal/attestation/attestationtest"
)

func TestPolicyWithMintedChains(t *testing.T) {
	ca := attestationtest.NewCA(t)
	v, err := attestation.NewVerifier(ca.RootPEM(), nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer := sha256.Sum256([]byte("agent signing cert"))
	base := attestationtest.Options{Challenge: []byte("nonce-1"), Package: "dev.armada.agent", SignerDigest: signer[:], OSPatchLevel: 202609}
	strict := attestation.Policy{Challenge: []byte("nonce-1"), RequireVerifiedBoot: true,
		AllowedPackages: []string{"dev.armada.agent"}, AllowedSigners: []string{hex.EncodeToString(signer[:])}}

	res, err := v.Verify(ca.Attest(&key.PublicKey, base), strict)
	if err != nil {
		t.Fatalf("locked, verified device rejected: %v", err)
	}
	if !res.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) || res.OSPatchLevel != 202609 {
		t.Fatalf("unexpected result %+v", res)
	}

	sb := base
	sb.SecurityLevel = attestation.StrongBox
	p := strict
	p.MinSecurityLevel = attestation.StrongBox
	if _, err := v.Verify(ca.Attest(&key.PublicKey, sb), p); err != nil {
		t.Fatalf("strongbox rejected: %v", err)
	}
	if _, err := v.Verify(ca.Attest(&key.PublicKey, base), p); err == nil {
		t.Fatal("TEE key accepted when StrongBox required")
	}
	unlocked := base
	unlocked.Unlocked = true
	if _, err := v.Verify(ca.Attest(&key.PublicKey, unlocked), strict); err == nil {
		t.Fatal("unlocked device accepted")
	}
	selfSigned := base
	selfSigned.BootState = attestation.BootSelfSigned
	if _, err := v.Verify(ca.Attest(&key.PublicKey, selfSigned), strict); err == nil {
		t.Fatal("custom ROM accepted")
	}
	other := base
	other.Package = "com.evil.clone"
	if _, err := v.Verify(ca.Attest(&key.PublicKey, other), strict); err == nil {
		t.Fatal("key from another app accepted")
	}
	// Without our CA, the same chain is rejected: only Google roots are trusted by default.
	google, _ := attestation.NewVerifier(nil, nil)
	if _, err := google.Verify(ca.Attest(&key.PublicKey, base), strict); err == nil {
		t.Fatal("chain from an untrusted CA accepted")
	}
}
