package env

import (
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiseConfigHasOnlyTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		tools []Tool
	}{
		{"plain", []Tool{{"node", "lts"}, {"go", "1.27.4"}}},
		{"empty", nil},
		{"name needing a quoted key", []Tool{{"npm:prettier", "3.3.3"}, {"asdf:https://example.test/plugin", "1"}}},
		{"hostile name", []Tool{{"go\"]\n[env]\nX = \"y", "1"}}},
		{"hostile version", []Tool{{"go", "1\n[env]\nX = \"y\"\n[tasks]"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got map[string]any

			require.NoError(t, toml.Unmarshal(MiseConfig(tt.tools), &got))

			for k := range got {
				assert.Equal(t, "tools", k)
			}

			tbl, _ := got["tools"].(map[string]any)
			for _, tool := range tt.tools {
				assert.Equal(t, tool.Version, tbl[tool.Name])
			}

			assert.Len(t, tbl, len(tt.tools))
		})
	}
}

func TestMiseConfigDeterministic(t *testing.T) {
	t.Parallel()

	a := MiseConfig([]Tool{{"node", "lts"}, {"go", "1"}})
	b := MiseConfig([]Tool{{"go", "1"}, {"node", "lts"}})
	assert.Equal(t, a, b)
}
