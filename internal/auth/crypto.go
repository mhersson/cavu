package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// ErrDecrypt reports a wrong key, truncated blob or tampered ciphertext; GCM
// authenticates, so none of them can yield garbage plaintext.
var ErrDecrypt = errors.New("auth: decrypt failed")

// EncryptSecret seals plaintext with AES-256-GCM as nonce || ciphertext, with
// a fresh random nonce per call.
func EncryptSecret(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce) // never returns an error

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// DecryptSecret reverses EncryptSecret and returns ErrDecrypt on any failure.
func DecryptSecret(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(blob) < gcm.NonceSize() {
		return nil, ErrDecrypt
	}

	nonce, ciphertext := blob[:gcm.NonceSize()], blob[gcm.NonceSize():]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecrypt
	}

	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("auth: encryption key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: gcm: %w", err)
	}

	return gcm, nil
}
