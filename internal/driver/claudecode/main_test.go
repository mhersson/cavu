package claudecode

import (
	"os"
	"testing"

	"github.com/mhersson/cavu/internal/testkit/fakeclaude"
)

// afterTests runs once the tests have run; a build-tagged file sets it to
// remove what its tests built.
var afterTests func()

// TestMain lets the test binary stand in for the claude CLI: with
// fakeclaude.EnvVar set it runs the fake and exits.
func TestMain(m *testing.M) {
	if _, ok := os.LookupEnv(fakeclaude.EnvVar); ok {
		fakeclaude.Main()
	}

	code := m.Run()

	if afterTests != nil {
		afterTests()
	}

	os.Exit(code)
}
