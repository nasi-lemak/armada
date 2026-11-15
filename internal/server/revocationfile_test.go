package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRevocationFile(t *testing.T) {
	s, _ := setup(t)
	f := filepath.Join(t.TempDir(), "status.json")
	os.WriteFile(f, []byte(`{"entries":{"00c35d2a":{"status":"REVOKED","reason":"KEY_COMPROMISE"}}}`), 0o600)
	if err := s.loadRevocationFile(f); err != nil {
		t.Fatal(err)
	}
	if s.revocations.Len() != 1 {
		t.Fatalf("loaded %d entries", s.revocations.Len())
	}
	os.WriteFile(f, []byte(`not json`), 0o600)
	if err := s.loadRevocationFile(f); err == nil {
		t.Fatal("bad file accepted")
	}
	if s.revocations.Len() != 1 {
		t.Fatal("a bad file replaced the list")
	}
}
