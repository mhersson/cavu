package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NewToken returns a 256-bit random token and its hash. Only the hash is ever
// stored, so a leaked database yields no usable token.
func NewToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("auth: generate token: %w", err)
	}

	raw = base64.RawURLEncoding.EncodeToString(buf)

	return raw, HashToken(raw), nil
}

// HashToken returns the hex SHA-256 of a raw token.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}
