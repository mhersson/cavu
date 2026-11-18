package engine_test

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerHalfHasNoRuntimeDependency(t *testing.T) {
	const module = "github.com/mhersson/cavu"

	out, err := execGoListDeps(t, module+"/internal/engine", module+"/internal/store", module+"/internal/replay",
		module+"/internal/auth", module+"/internal/api")
	require.NoError(t, err, "%s", out)
	assert.Empty(t, runtimeDeps(strings.Fields(out)))

	assert.Equal(t, []string{module + "/internal/driver/claudecode", module + "/internal/bridge", module + "/internal/forge"}, runtimeDeps([]string{
		"context", module + "/internal/github", module + "/internal/driver/claudecode", module + "/internal/gitlab", module + "/internal/bridge",
		module + "/internal/forge",
	}), "the check fires on a denied package and nothing else")
}

// runtimeDeps returns the import paths that belong to the runtime half, and
// the forge's, which the server half never calls.
func runtimeDeps(paths []string) []string {
	var hits []string

	for _, p := range paths {
		for _, d := range []string{"sandbox", "runner", "proxy", "sidecar", "git", "driver", "bridge", "forge"} {
			deny := "github.com/mhersson/cavu/internal/" + d
			if p == deny || strings.HasPrefix(p, deny+"/") {
				hits = append(hits, p)
			}
		}
	}

	return hits
}

func execGoListDeps(t *testing.T, pkgs ...string) (string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list", "-deps"}, pkgs...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("go list: %w", err)
	}

	return stdout.String(), nil
}
