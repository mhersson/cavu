package engine_test

import (
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBranchSlug(t *testing.T) {
	long := strings.Repeat("Fix bug ", 37) + "abcd"
	require.Len(t, long, 300)
	assert.Len(t, engine.Slug(long), 50)

	tests := []struct {
		name, title, want string
	}{
		{"plain title", "Fix the login bug", "cavu/fix-the-login-bug"},
		{"shell metacharacters", "; rm -rf / #", "cavu/rm-rf"},
		{"leading dashes", "--upload-pack=x", "cavu/upload-pack-x"},
		{"dot run", "a..b", "cavu/a-b"},
		{"300 characters", long, "cavu/" + strings.Repeat("fix-bug-", 6) + "fi"},
		{"cut on a separator", strings.Repeat("a", 49) + " tail", "cavu/" + strings.Repeat("a", 49)},
		{"non-ASCII letters", "Ærlig talt", "cavu/rlig-talt"},
		{"only punctuation", "!!!", "cavu/task"},
		{"empty", "", "cavu/task"},
		{"newline", "fix\nlogin", "cavu/fix-login"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, engine.BranchName(tc.title))
		})
	}
}
