package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/store"
)

// The kinds of a named credential.
const (
	CredentialToken = "token" // a personal access token
	CredentialApp   = "app"   // a GitHub App
)

// CredentialSecretName is the vault name of a named credential's secret:
// forge.<name>.token, or forge.<name>.app_key for an App.
func CredentialSecretName(name, kind string) string {
	if kind == CredentialApp {
		return credentialSecretPrefix(name) + "app_key"
	}

	return credentialSecretPrefix(name) + "token"
}

func credentialSecretPrefix(name string) string { return "forge." + name + "." }

// NewCredential is a named credential written whole: Token for a personal
// access token, or AppID with AppKey for a GitHub App.
type NewCredential struct {
	Token  string
	AppID  int64
	AppKey string
}

func (c NewCredential) kind() string {
	if c.Token != "" {
		return CredentialToken
	}

	return CredentialApp
}

func (c NewCredential) valid() bool {
	if c.Token != "" {
		return c.AppID == 0 && c.AppKey == ""
	}

	return c.AppID >= 1 && c.AppKey != ""
}

// ChangeCredentials checks every change, then writes them: a nil change removes its name.
func ChangeCredentials(ctx context.Context, s Store, v *auth.Vault, changes map[string]*NewCredential) error {
	known, err := s.Credentials(ctx)
	if err != nil {
		return err
	}

	projects, err := s.Projects(ctx)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(changes))
	for name := range changes {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		c := changes[name]

		switch {
		case !projectName.MatchString(name):
			return refuse("credential %q: want 1 to 40 of a-z, 0-9 and -", name)
		case c != nil && !c.valid():
			return refuse("credential %q: want a token alone, or an App id of 1 or more with its key", name)
		case c == nil:
			if err := checkRemoval(name, known, projects); err != nil {
				return err
			}
		}
	}

	for _, name := range names {
		if c := changes[name]; c == nil {
			if err := s.RemoveCredential(ctx, name, credentialSecretPrefix(name)); err != nil {
				return err
			}
		} else if err := putCredential(ctx, s, v, name, *c); err != nil {
			return err
		}
	}

	return nil
}

// checkRemoval refuses the removal of a credential no one has or a live project names.
func checkRemoval(name string, known []store.Credential, projects []store.Project) error {
	if !slices.ContainsFunc(known, func(c store.Credential) bool { return c.Name == name }) {
		return refuse("credential %q does not exist", name)
	}

	var using []string

	for _, p := range projects {
		if p.ForgeCredential == name {
			using = append(using, p.Name)
		}
	}

	if len(using) > 0 {
		return refuse("credential %q is named by project %s; change the project first", name, strings.Join(using, ", "))
	}

	return nil
}

// putCredential seals the secret before it writes the row, so a row's kind
// never lacks its secret, and drops the other kind's secret after.
func putCredential(ctx context.Context, s Store, v *auth.Vault, name string, c NewCredential) error {
	kind, secret := c.kind(), c.Token
	if kind == CredentialApp {
		secret = c.AppKey
	}

	if err := v.Set(ctx, CredentialSecretName(name, kind), secret); err != nil {
		return fmt.Errorf("credential %s: %w", name, err)
	}

	if err := s.PutCredential(ctx, store.Credential{Name: name, Kind: kind, AppID: c.AppID}); err != nil {
		return err
	}

	other := CredentialToken
	if kind == CredentialToken {
		other = CredentialApp
	}

	return v.Delete(ctx, CredentialSecretName(name, other))
}
