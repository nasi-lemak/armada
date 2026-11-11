package totp

import (
	"encoding/base32"
	"testing"
	"time"
)

// RFC 6238 appendix B test vectors (SHA-1, last 6 of the 8 digits).
func TestRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		got, err := Code(secret, unix/30)
		if err != nil || got != want {
			t.Errorf("t=%d: got %s want %s", unix, got, want)
		}
	}
}

func TestVerifySkewAndReplay(t *testing.T) {
	s := NewSecret()
	now := time.Unix(1_800_000_000, 0)
	prev, _ := Code(s, Step(now)-1)
	step, ok := Verify(s, prev, now, 0)
	if !ok || step != Step(now)-1 {
		t.Fatal("previous step not accepted")
	}
	if _, ok := Verify(s, prev, now, step); ok {
		t.Fatal("replayed code accepted")
	}
	old, _ := Code(s, Step(now)-3)
	if _, ok := Verify(s, old, now, 0); ok {
		t.Fatal("stale code accepted")
	}
	if _, ok := Verify(s, "12345", now, 0); ok {
		t.Fatal("short code accepted")
	}
}
