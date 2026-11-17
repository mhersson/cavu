//go:build integration

package doctor

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/testkit/mockapi"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

func TestMain(m *testing.M) { sandboxtest.Main(m) }

// TestDoctorAgainstMock runs every check on the built image with the pinned
// claude, in key mode against the mock. Without a forge token, forge is the
// one check skipped.
func TestDoctorAgainstMock(t *testing.T) {
	r := sandboxtest.Runtime(t)
	image := sandboxtest.Image(t, r, claudecode.Version, claudecode.SHA512, nil)
	mock := mockapi.Start(t, []mockapi.Step{
		{Tool: "Bash", Input: map[string]any{"command": "echo cavu"}},
		{Tool: mockapi.Structured, Input: map[string]any{"status": "done"}},
		{Tool: mockapi.Structured, Input: map[string]any{"status": "done"}},
	})

	b := make([]byte, 8)
	_, _ = rand.Read(b)
	instance := hex.EncodeToString(b)

	sandboxtest.RemoveInstanceAfter(t, r, instance)

	credential := "doctor-credential-" + instance
	checks := Run(t.Context(), Deps{
		Version:      strings.TrimPrefix(image, "cavu-sandbox:"),
		AgentVersion: claudecode.Version,
		Settings: map[string]string{
			"runtime": r.Kind, "instance_id": instance, "endpoint_mode": "key", "endpoint_url": mock.HostURL(),
			"endpoint_auth_header": "bearer", "model_work": "claude-haiku-5-5", "commit_name": "CAVU", "commit_email": "cavu@example.com",
		},
		Credential: credential,
		Driver:     claudecode.New(),
		host:       &runtimeHost{image: image, instance: instance, labels: sandboxtest.Labels()},
	})

	for _, c := range byName(t, checks) {
		if c.Name == "forge" {
			assert.True(t, c.Skipped, "%+v", c)

			continue
		}

		assert.True(t, c.OK, "%+v", c)
	}

	assert.Equal(t, 0, ExitCode(checks))

	// The tool call and the structured result, then the resumed turn's result,
	// each through the sidecar with the real credential.
	var calls int

	for _, req := range mock.Requests() {
		if req.ModelCall {
			calls++

			assert.Equal(t, "Bearer "+credential, req.Header.Get("Authorization"))
		}
	}

	assert.Equal(t, 3, calls)
}
