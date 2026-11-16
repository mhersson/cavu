package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHosts(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{"subscription", modeSubscription, []string{"api.anthropic.com"}},
		{"key", modeKey, nil},
		{"unknown", "other", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := claudeCode{}.Hosts(tt.mode)
			if len(tt.want) == 0 {
				assert.Empty(t, got)

				return
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHostsReturnsFreshSlice(t *testing.T) {
	c := claudeCode{}

	first := c.Hosts(modeSubscription)
	first[0] = "changed.example"
	_ = append(first, "extra.example")

	assert.Equal(t, []string{"api.anthropic.com"}, c.Hosts(modeSubscription))
}
