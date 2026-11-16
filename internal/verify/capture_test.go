package verify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArgv(t *testing.T) {
	assert.Equal(t, []string{"bash", "-c", "set -o pipefail; make test | tee x"}, Argv("make test | tee x"))
}
