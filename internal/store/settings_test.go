package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettings(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, ok, err := s.Setting(ctx, "runtime")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, s.SetSetting(ctx, "runtime", "docker"))
	require.NoError(t, s.SetSetting(ctx, "model_work", "m"))
	require.NoError(t, s.SetSetting(ctx, "runtime", "podman"))

	v, ok, err := s.Setting(ctx, "runtime")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "podman", v)

	all, err := s.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"runtime": "podman", "model_work": "m"}, all)
}
