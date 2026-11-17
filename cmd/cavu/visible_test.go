package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
)

func TestVisibleWriter(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"carriage return", []string{"a\rb"}, "a^Mb"},
		{"escape sequence", []string{"\x1b[2K"}, "^[[2K"},
		{"C1 control", []string{"a\u009bb"}, "aM-^[b"},
		{"DEL", []string{"\x7f"}, "^?"},
		{"NUL", []string{"\x00"}, "^@"},
		{"tab and newline", []string{"a\tb\n"}, "a\tb\n"},
		{"multi-byte text", []string{"é ✓\n"}, "é ✓\n"},
		{"stray C1 byte", []string{"\x9b"}, "M-^["},
		{"other stray byte", []string{"\xff"}, "\xff"},
		{"C1 split across writes", []string{"\xc2", "\x9b"}, "M-^["},
		{"character split across writes", []string{"\xe2\x9c", "\x93"}, "✓"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			v := &visibleWriter{w: &out}

			for _, w := range tt.writes {
				n, err := v.Write([]byte(w))
				require.NoError(t, err)
				assert.Equal(t, len(w), n)
			}

			assert.Equal(t, tt.want, out.String())
		})
	}
}

func TestRunKeepsANonTerminalStreamByteExact(t *testing.T) {
	dir := webData(t)
	id := addTask(t, dir, "Fix")
	art := t.TempDir()
	diff := filepath.Join(art, "new.diff")
	raw := "+a\r\x1b[2K\u009b\x7f\xff\n"
	require.NoError(t, os.WriteFile(diff, []byte(raw), 0o600))
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}),
		event(engine.Exported, "runner", engine.ExportedPayload{DiffPath: diff}))

	assert.Equal(t, raw, cavuIn(t, dir, "task", "diff", id))
}
