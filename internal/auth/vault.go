package auth

import (
	"context"
	"fmt"

	"github.com/mhersson/cavu/internal/store"
)

const purposeSecrets = "secrets"

// Vault keeps named secrets sealed in the store under a key derived from the
// master key.
type Vault struct {
	store *store.Store
	key   []byte
}

// NewVault returns a vault sealing with the "secrets" subkey of master.
func NewVault(s *store.Store, master []byte) (*Vault, error) {
	key, err := DeriveKey(master, purposeSecrets)
	if err != nil {
		return nil, err
	}

	return &Vault{store: s, key: key}, nil
}

// Set seals value and stores it under name, replacing any previous value.
func (v *Vault) Set(ctx context.Context, name, value string) error {
	sealed, err := EncryptSecret(v.key, []byte(value))
	if err != nil {
		return err
	}

	return v.store.PutSecret(ctx, name, sealed)
}

// Get returns the plaintext stored under name; false when unset. A blob that
// does not decrypt is an error wrapping ErrDecrypt.
func (v *Vault) Get(ctx context.Context, name string) (string, bool, error) {
	sealed, ok, err := v.store.Secret(ctx, name)
	if err != nil || !ok {
		return "", false, err
	}

	plain, err := DecryptSecret(v.key, sealed)
	if err != nil {
		return "", false, fmt.Errorf("secret %q: %w", name, err)
	}

	return string(plain), true, nil
}

// Delete removes the secret name; an unknown name is a no-op.
func (v *Vault) Delete(ctx context.Context, name string) error {
	return v.store.DeleteSecret(ctx, name)
}

// Values returns the plaintext of every stored secret, for the redactor.
func (v *Vault) Values(ctx context.Context) ([]string, error) {
	names, err := v.store.SecretNames(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(names))

	for _, name := range names {
		val, ok, err := v.Get(ctx, name)
		if err != nil {
			return nil, err
		}

		if ok {
			out = append(out, val)
		}
	}

	return out, nil
}
