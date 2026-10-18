// Package secretbox encrypts webhook signing secrets at rest (ADR 0014). The notifier needs the
// plaintext to sign, so it can't store a hash; AES-256-GCM with a key from the environment
// means a copy of the database alone can't forge webhooks.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Box seals and opens secrets with one key.
type Box struct {
	aead cipher.AEAD
}

// New takes the key as standard base64 of exactly 32 bytes (NOTIFIER_SECRET_KEY).
func New(keyBase64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, fmt.Errorf("NOTIFIER_SECRET_KEY is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("NOTIFIER_SECRET_KEY must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh random nonce. associated (the endpoint id) is
// authenticated but not encrypted: the ciphertext only opens for the same endpoint.
func (b *Box) Seal(plaintext, associated []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, plaintext, associated), nonce, nil
}

// ErrOpen means the ciphertext, nonce, key or associated data don't match.
var ErrOpen = errors.New("secret doesn't decrypt")

// Open decrypts what Seal produced.
func (b *Box) Open(ciphertext, nonce, associated []byte) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, ErrOpen
	}
	plain, err := b.aead.Open(nil, nonce, ciphertext, associated)
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}
