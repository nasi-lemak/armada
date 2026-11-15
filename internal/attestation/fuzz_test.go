package attestation_test

import (
	"testing"

	"github.com/nasi-lemak/armada/internal/attestation"
	"github.com/nasi-lemak/armada/internal/attestation/attestationtest"
)

// The attestation extension comes from the device: it must never crash the
// parser, whatever it holds.
func FuzzParseKeyDescription(f *testing.F) {
	f.Add(attestationtest.KeyDescription(attestationtest.Options{Package: "com.acme.app", SignerDigest: make([]byte, 32), Challenge: []byte("nonce"), OSPatchLevel: 202501}))
	f.Add(attestationtest.KeyDescription(attestationtest.Options{Unlocked: true, BootState: attestation.BootState(2)}))
	f.Add([]byte{0x30, 0x00})
	f.Add([]byte{0x30, 0x84, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x30, 0x81})
	f.Fuzz(func(t *testing.T, der []byte) {
		kd, err := attestation.ParseKeyDescription(der)
		if err == nil && kd == nil {
			t.Fatal("nil description without an error")
		}
	})
}
