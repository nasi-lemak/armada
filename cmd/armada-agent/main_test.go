package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "agent.log")
	w, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 25; i++ {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	cur, _ := os.Stat(path)
	old, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("no rotated file")
	}
	if cur.Size() > maxLogSize || old.Size() > maxLogSize {
		t.Errorf("log grew past the limit: %d and %d bytes", cur.Size(), old.Size())
	}
	if cur.Size()+old.Size() < 10<<20 {
		t.Errorf("too little kept: %d bytes", cur.Size()+old.Size())
	}
	// Reopening an oversized file starts it over.
	os.WriteFile(path, bytes.Repeat([]byte("y"), maxLogSize+1), 0o600)
	w, err = openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("fresh\n"))
	w.Close()
	if b, _ := os.ReadFile(path); string(b) != "fresh\n" {
		t.Errorf("oversized log not rotated on open: %d bytes", len(b))
	}
}
