package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCredentialRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	require.NoError(t, s.PutCredential(ctx, Credential{Name: "zed", Kind: "app", AppID: 42}))
	require.NoError(t, s.PutCredential(ctx, Credential{Name: "acme", Kind: "token"}))

	got, err := s.Credentials(ctx)
	require.NoError(t, err)
	require.Equal(t, []Credential{{Name: "acme", Kind: "token"}, {Name: "zed", Kind: "app", AppID: 42}}, got)

	require.NoError(t, s.PutCredential(ctx, Credential{Name: "acme", Kind: "app", AppID: 7}))

	got, err = s.Credentials(ctx)
	require.NoError(t, err)
	require.Equal(t, []Credential{{Name: "acme", Kind: "app", AppID: 7}, {Name: "zed", Kind: "app", AppID: 42}}, got)
}

func TestRemoveCredentialRefusedWhileNamed(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	require.NoError(t, s.PutCredential(ctx, Credential{Name: "acme", Kind: "token"}))
	require.NoError(t, s.PutCredential(ctx, Credential{Name: "acme-org", Kind: "token"}))

	for _, name := range []string{"forge.acme.token", "forge.acme-org.token", "forge_token"} {
		require.NoError(t, s.PutSecret(ctx, name, []byte("v")))
	}

	p, err := s.CreateProject(ctx, Project{Name: "demo", ForgeCredential: "acme"})
	require.NoError(t, err)

	require.Error(t, s.RemoveCredential(ctx, "acme", "forge.acme."))

	creds, err := s.Credentials(ctx)
	require.NoError(t, err)
	require.Len(t, creds, 2)

	names, err := s.SecretNames(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"forge.acme-org.token", "forge.acme.token", "forge_token"}, names)

	require.NoError(t, s.RemoveProject(ctx, p.ID, time.Now(), "project.demo."))
	require.NoError(t, s.RemoveCredential(ctx, "acme", "forge.acme."))

	creds, err = s.Credentials(ctx)
	require.NoError(t, err)
	require.Equal(t, []Credential{{Name: "acme-org", Kind: "token"}}, creds)

	names, err = s.SecretNames(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"forge.acme-org.token", "forge_token"}, names)
}

func TestRemoveUnknownCredential(t *testing.T) {
	t.Parallel()

	require.ErrorIs(t, openTest(t).RemoveCredential(context.Background(), "nope", "forge.nope."), ErrNotFound)
}
