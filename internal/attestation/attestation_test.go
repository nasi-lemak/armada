package attestation

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The testdata chains are real device attestations published in
// github.com/google/android-key-attestation (Apache 2.0). They were made on
// test devices with unlocked bootloaders and challenge "abc".

func loadChain(t *testing.T, dir string) [][]byte {
	t.Helper()
	var chain [][]byte
	for i := 0; ; i++ {
		b, err := os.ReadFile(filepath.Join("testdata", dir, fmt.Sprintf("cert%d.pem", i)))
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(b)
		chain = append(chain, block.Bytes)
	}
	return chain
}

func verifier(t *testing.T, revoked func(string) (bool, string)) *Verifier {
	t.Helper()
	v, err := NewVerifier(nil, revoked)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGoogleRootsLoaded(t *testing.T) {
	v := verifier(t, nil)
	// Google's RSA root (published several times with the same key) and the EC "Key Attestation CA1" root.
	if len(v.roots) != 2 {
		t.Fatalf("expected 2 distinct root keys, got %d", len(v.roots))
	}
}

func TestVerifyRealTEEChains(t *testing.T) {
	for _, dir := range []string{"EC_SecurityLevel_TEE", "RSA_SecurityLevel_TEE"} {
		t.Run(dir, func(t *testing.T) {
			res, err := verifier(t, nil).Verify(loadChain(t, dir), Policy{Challenge: []byte("abc")})
			if err != nil {
				t.Fatal(err)
			}
			kd := res.KeyDescription
			if kd.AttestationVersion != 3 || kd.KeymasterVersion != 4 {
				t.Errorf("versions: attestation %d keymaster %d", kd.AttestationVersion, kd.KeymasterVersion)
			}
			if res.SecurityLevel != TrustedEnvironment || kd.KeymasterSecurityLevel != TrustedEnvironment {
				t.Errorf("security level %s / %s", res.SecurityLevel, kd.KeymasterSecurityLevel)
			}
			if len(kd.UniqueID) != 0 {
				t.Errorf("unique id %x", kd.UniqueID)
			}
			if res.RootOfTrust.VerifiedBootState != BootUnverified || res.RootOfTrust.DeviceLocked {
				t.Errorf("root of trust: %+v", res.RootOfTrust)
			}
			switch {
			case strings.HasPrefix(dir, "EC"):
				if _, ok := res.PublicKey.(*ecdsa.PublicKey); !ok {
					t.Errorf("public key type %T", res.PublicKey)
				}
			default:
				if _, ok := res.PublicKey.(*rsa.PublicKey); !ok {
					t.Errorf("public key type %T", res.PublicKey)
				}
			}
			if res.ApplicationID == nil || len(res.ApplicationID.Packages) == 0 {
				t.Errorf("no application id parsed")
			} else {
				t.Logf("app id: %+v, %d signer digest(s), os %d patch %d", res.ApplicationID.Packages, len(res.ApplicationID.SignatureDigests), res.OSVersion, res.OSPatchLevel)
			}
		})
	}
}

func TestVerifyRejections(t *testing.T) {
	v := verifier(t, nil)
	chain := loadChain(t, "EC_SecurityLevel_TEE")

	cases := []struct {
		name   string
		chain  [][]byte
		policy Policy
		want   string
	}{
		{"wrong challenge", chain, Policy{Challenge: []byte("abd")}, "challenge"},
		{"unlocked bootloader", chain, Policy{Challenge: []byte("abc"), RequireVerifiedBoot: true}, "boot state"},
		{"needs strongbox", chain, Policy{Challenge: []byte("abc"), MinSecurityLevel: StrongBox}, "need at least strongbox"},
		{"wrong app", chain, Policy{Challenge: []byte("abc"), AllowedPackages: []string{"com.example.nope"}}, "allowed app"},
		{"wrong signer", chain, Policy{Challenge: []byte("abc"), AllowedSigners: []string{strings.Repeat("ab", 32)}}, "signing certificate"},
		{"untrusted root", loadChain(t, "EC_SecurityLevel_StrongBox"), Policy{Challenge: []byte("abc")}, ""},
		{"truncated chain", chain[:1], Policy{Challenge: []byte("abc")}, "too short"},
		{"missing root", chain[:3], Policy{Challenge: []byte("abc")}, ""},
		{"reordered", [][]byte{chain[1], chain[0], chain[2], chain[3]}, Policy{Challenge: []byte("abc")}, "not signed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Verify(c.chain, c.policy)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestVerifyAllowedAppAndSigner(t *testing.T) {
	v := verifier(t, nil)
	chain := loadChain(t, "EC_SecurityLevel_TEE")
	res, err := v.Verify(chain, Policy{Challenge: []byte("abc")})
	if err != nil {
		t.Fatal(err)
	}
	app := res.ApplicationID
	p := Policy{Challenge: []byte("abc"), AllowedPackages: []string{app.Packages[0].Name}}
	if len(app.SignatureDigests) > 0 {
		// keytool prints digests as colon-separated uppercase hex; accept that form.
		hexd := strings.ToUpper(fmt.Sprintf("%x", app.SignatureDigests[0]))
		var parts []string
		for i := 0; i < len(hexd); i += 2 {
			parts = append(parts, hexd[i:i+2])
		}
		p.AllowedSigners = []string{strings.Join(parts, ":")}
	}
	if _, err := v.Verify(chain, p); err != nil {
		t.Fatal(err)
	}
}

func TestRevocation(t *testing.T) {
	chain := loadChain(t, "EC_SecurityLevel_TEE")
	var rl RevocationList
	// Revoke an intermediate by its certificate serial (lowercase hex, no leading zeros, as Google lists it).
	v0 := verifier(t, nil)
	if _, err := v0.Verify(chain, Policy{Challenge: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	doc := `{"entries": {"388266760658996857d": {"status": "REVOKED", "reason": "KEY_COMPROMISE"}}}`
	if err := rl.Load(strings.NewReader(doc)); err != nil {
		t.Fatal(err)
	}
	_, err := verifier(t, rl.IsRevoked).Verify(chain, Policy{Challenge: []byte("abc")})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("expected revocation error, got %v", err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, b := range [][]byte{nil, {0x30}, {0x30, 0x05, 0x02}, {0x30, 0x84, 0xff, 0xff, 0xff, 0xff}} {
		if _, err := ParseKeyDescription(b); err == nil {
			t.Errorf("parsed garbage %x", b)
		}
	}
}
