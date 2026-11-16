package proc

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxOnlyRefusesOutsideASandbox(t *testing.T) {
	if _, err := os.Stat(SandboxMarker); err == nil {
		t.Skip("this host carries the sandbox marker")
	}

	cmd := exec.CommandContext(t.Context(), "sh", "-c", SandboxOnly("echo payload-ran"))
	cmd.Dir = t.TempDir()

	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError

	require.ErrorAs(t, err, &ee)
	assert.Equal(t, NotInSandbox, ee.ExitCode())
	assert.NotContains(t, string(out), "payload-ran")
}
