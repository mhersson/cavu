// Package auth holds the master key, the AES-256-GCM sealing built on it and
// the vault that keeps named secrets sealed in the store.
package auth

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MasterKeyLen is the byte length of a master key, stored hex-encoded.
const MasterKeyLen = 32

// ErrKeyFileMode reports a master key file readable by group or others.
var ErrKeyFileMode = errors.New("auth: master key file permissions too open")

// LoadMasterKey reads a hex-encoded master key from path. It never creates
// anything: a missing file wraps os.ErrNotExist, and a file with any group or
// other permission is refused with ErrKeyFileMode.
func LoadMasterKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read master key: %w", err)
	}

	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is %#o, want 0600", ErrKeyFileMode, path, info.Mode().Perm())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read master key: %w", err)
	}

	decoded, decErr := hex.DecodeString(strings.TrimSpace(string(data)))
	if decErr != nil || len(decoded) != MasterKeyLen {
		return nil, fmt.Errorf("auth: master key file %s: want %d hex-encoded bytes", path, MasterKeyLen)
	}

	return decoded, nil
}

// LoadOrCreateMasterKey loads the key at path, or generates one with 0600
// permissions in a 0700 parent directory when the file does not exist.
func LoadOrCreateMasterKey(path string) (key []byte, created bool, err error) {
	key, err = LoadMasterKey(path)

	switch {
	case err == nil:
		return key, false, nil

	case errors.Is(err, os.ErrNotExist):
		fresh := make([]byte, MasterKeyLen)
		if _, err := rand.Read(fresh); err != nil {
			return nil, false, fmt.Errorf("auth: generate master key: %w", err)
		}

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, false, fmt.Errorf("auth: master key dir: %w", err)
		}

		if err := os.WriteFile(path, []byte(hex.EncodeToString(fresh)+"\n"), 0o600); err != nil {
			return nil, false, fmt.Errorf("auth: write master key: %w", err)
		}

		return fresh, true, nil

	default:
		return nil, false, err
	}
}

// DeriveKey derives a purpose-labeled 32-byte subkey via HKDF-SHA256, so the
// master key is never used directly and purposes never share key material.
func DeriveKey(master []byte, purpose string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, master, nil, purpose, 32)
	if err != nil {
		return nil, fmt.Errorf("auth: derive %s key: %w", purpose, err)
	}

	return key, nil
}
