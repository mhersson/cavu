package gitrepo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An environment of only repository variables cleans to an empty one, never
// to nil, which would hand git the whole inherited environment.
func TestCleanEnvIsNeverNil(t *testing.T) {
	env := cleanEnv(t, []string{"GIT_DIR=/elsewhere/.git", "GIT_WORK_TREE=/elsewhere", "GIT_CONFIG_KEY_0=core.bare"})

	assert.NotNil(t, env)
	assert.Empty(t, env)
}
