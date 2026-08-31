package webhook

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// ErrCipherSealed reports a body that cannot be decrypted with the
// configured key: tampered ciphertext, wrong key, or truncated storage.
var ErrCipherSealed = errors.New("webhook payload cannot be decrypted")

// PayloadCipher encrypts raw webhook bodies at rest (secrets-webhooks
// section 9: raw body encrypted, access-restricted, retention-bounded).
// AES-256-GCM with a random 12-byte nonce prefixed to the ciphertext; the
// key material is derived with SHA-256 so any non-empty configured secret
// is acceptable without pinning operators to exact byte lengths.
type PayloadCipher struct {
	aead cipher.AEAD
}

// NewPayloadCipher derives the AEAD from server-held key material. Empty
// material is a construction error: the receiver stays unmounted rather
// than persisting plaintext bodies (honest degradation, never a fake).
func NewPayloadCipher(keyMaterial string) (*PayloadCipher, error) {
	if keyMaterial == "" {
		return nil, errors.New("webhook payload key material must not be empty")
	}
	key := sha256.Sum256([]byte(keyMaterial))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("webhook payload cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webhook payload cipher: %w", err)
	}
	return &PayloadCipher{aead: aead}, nil
}

// Seal encrypts the raw body. Distinct deliveries get distinct nonces, so
// identical payloads never yield comparable ciphertext.
func (c *PayloadCipher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("webhook payload nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

// Open decrypts a sealed body, failing closed on any tampering.
func (c *PayloadCipher) Open(sealed []byte) ([]byte, error) {
	size := c.aead.NonceSize()
	if len(sealed) <= size {
		return nil, ErrCipherSealed
	}
	plain, err := c.aead.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		return nil, ErrCipherSealed
	}
	return plain, nil
}
