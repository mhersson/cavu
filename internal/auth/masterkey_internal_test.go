package auth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateMasterKeyExistingFileWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	hexKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	require.NoError(t, os.WriteFile(path, []byte(hexKey+"\n"), 0o600))

	key, created, err := createMasterKey(path)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, byte(0x1f), key[31])

	again, err := LoadMasterKey(path)
	require.NoError(t, err)
	assert.Equal(t, key, again, "the file was not overwritten")
}
