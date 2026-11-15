package license

import (
	"crypto/ed25519"
	"testing"
	"time"
)

// Your backend verifies tokens sent by apps; a forged or garbled one must
// be refused, never crash the verifier or verify.
func FuzzVerify(f *testing.F) {
	pub, _, _ := ed25519.GenerateKey(nil)
	v := NewVerifier("com.acme.app", pub)
	for _, s := range []string{"a.b.c", "", "..", "eyJhbGciOiJFZERTQSJ9.eyJhdWQiOiJjb20uYWNtZS5hcHAifQ.AAAA",
		"eyJhbGciOiJub25lIn0.eyJhdWQiOiJjb20uYWNtZS5hcHAifQ."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, token string) {
		if c, err := v.Verify(token, time.Now()); err == nil {
			t.Fatalf("random token %q verified: %+v", token, c)
		}
	})
}
