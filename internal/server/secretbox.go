package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// secretBox encrypts small secrets stored in the database (authenticator
// secrets) with a key kept in the data directory, so a copy of the
// database alone doesn't reveal them.
type secretBox struct{ aead cipher.AEAD }

func loadSecretBox(path string) (*secretBox, error) {
	var key []byte
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(b))); err != nil || len(key) != 32 {
			return nil, fmt.Errorf("secrets key %s is corrupt", path)
		}
	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secretBox{aead}, nil
}

func (b *secretBox) seal(plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

func (b *secretBox) open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", errors.New("corrupt secret")
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("secret does not decrypt (wrong secrets.key?)")
	}
	return string(plain), nil
}
