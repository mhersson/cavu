package claudecode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeLineToleratesChangedFieldTypes(t *testing.T) {
	l, ok := decodeLine([]byte(`{"type":"result","num_turns":"three","is_error":true,"x_new":{"a":1}}`))
	require.True(t, ok)
	assert.Equal(t, "result", l.Type)
	assert.True(t, l.IsError)
	assert.Zero(t, l.NumTurns)

	for _, bad := range []string{"not json", ``, `{"type":`} {
		_, ok := decodeLine([]byte(bad))
		assert.False(t, ok, bad)
	}

	for _, other := range []string{`"a string"`, `[1,2]`} {
		l, _ := decodeLine([]byte(other))
		assert.Empty(t, l.Type, other)
	}
}
