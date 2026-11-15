package certs

import "testing"

// Certificates, requests and keys are uploaded by people and reported by
// devices: parsing must fail cleanly, never crash.
func FuzzParse(f *testing.F) {
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"))
	f.Add([]byte("-----BEGIN CERTIFICATE REQUEST-----\n-----END CERTIFICATE REQUEST-----\n"))
	f.Add([]byte("-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----\n"))
	f.Add([]byte{0x30, 0x82, 0x01, 0x00})
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) {
		if cs, err := ParseCerts(b); err == nil {
			for _, c := range cs {
				_ = Describe(c)
			}
		}
		_, _ = ParseCSR(b)
		_, _ = ParseKey(b)
	})
}
