package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultBinaryOnLinux(t *testing.T) {
	self := func() (string, error) { return "/run/cavu", nil }

	got, err := defaultBinary("linux", "", self)
	require.NoError(t, err)
	assert.Equal(t, "/run/cavu", got)

	got, err = defaultBinary("linux", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	_, err = defaultBinary("darwin", "", self)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--cavu-binary")

	got, err = defaultBinary("darwin", "/x/cavu", self)
	require.NoError(t, err)
	assert.Equal(t, "/x/cavu", got)

	_, err = defaultBinary("linux", "", func() (string, error) { return "", errors.New("no") })
	require.Error(t, err)
}

func TestImageVerbRefusesOtherActions(t *testing.T) {
	code, _, stderr := runCavu("image", "frobnicate")

	assert.Equal(t, 1, code)
	assert.NotEmpty(t, stderr)
}
