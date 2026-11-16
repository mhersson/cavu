package claudecode

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

func TestProbe(t *testing.T) {
	// The caller compares the version with its pin, so any release the
	// binary reports comes back without an error.
	for _, version := range []string{"2.1.325", "9.0.1"} {
		t.Run(version, func(t *testing.T) {
			d := testDriver(t)
			target := newLocalTarget(t, fakeEnv(t, fakeclaude.Scenario{Version: version})...)

			got, err := d.Probe(bounded(t), target)
			require.NoError(t, err)
			assert.Equal(t, version, got)

			cmds := target.started()
			require.Len(t, cmds, 1)
			assert.Equal(t, []string{d.binary, "--version"}, cmds[0].Argv)
		})
	}

	t.Run("binary fails", func(t *testing.T) {
		target := newLocalTarget(t, fakeclaude.EnvVar+"="+filepath.Join(t.TempDir(), "missing.json"))

		_, err := testDriver(t).Probe(bounded(t), target)
		require.Error(t, err)
	})

	t.Run("unrecognised output", func(t *testing.T) {
		target := newLocalTarget(t, fakeEnv(t, fakeclaude.Scenario{Version: "beta"})...)

		_, err := testDriver(t).Probe(bounded(t), target)
		require.Error(t, err)
	})
}
