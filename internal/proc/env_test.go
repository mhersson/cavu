package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildEnv(t *testing.T) {
	t.Parallel()

	got := BuildEnv(
		[]string{"PATH=/bin", "HOME=/old", "KEEP=1"},
		map[string]string{"HOME": "/new", "B": "2", "A": "1"},
	)

	assert.Equal(t, []string{"PATH=/bin", "KEEP=1", "A=1", "B=2", "HOME=/new"}, got)
}
