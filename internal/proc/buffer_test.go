package proc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuffer(t *testing.T) {
	for name, tc := range map[string]struct {
		writes  []string
		kept    string
		dropped bool
	}{
		"nothing":                  {kept: ""},
		"under the limit":          {writes: []string{"ab", "c"}, kept: "abc"},
		"at the limit":             {writes: []string{"ab", "cd"}, kept: "abcd"},
		"one write past it":        {writes: []string{"abcdef"}, kept: "abcd", dropped: true},
		"a later write past it":    {writes: []string{"abc", "de", "f"}, kept: "abcd", dropped: true},
		"an empty write when full": {writes: []string{"abcd", ""}, kept: "abcd"},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer(4)

			for _, w := range tc.writes {
				n, err := b.Write([]byte(w))
				require.NoError(t, err)
				assert.Equal(t, len(w), n, "a write reports all its bytes taken")
			}

			assert.Equal(t, tc.kept, string(b.Bytes()))
			assert.Equal(t, tc.dropped, b.Dropped())
		})
	}
}
