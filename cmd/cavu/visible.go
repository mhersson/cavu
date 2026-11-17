package main

import (
	"io"
	"slices"
	"unicode/utf8"
)

// visibleWriter shows control characters other than newline and tab in cat -v
// notation, so agent-written text cannot move the cursor or rewrite the screen.
type visibleWriter struct {
	w    io.Writer
	held []byte // an incomplete UTF-8 sequence ending the previous write
}

// A sequence still held when the process ends is dropped: fmt never writes a partial rune.
func (v *visibleWriter) Write(p []byte) (int, error) {
	in := slices.Concat(v.held, p)
	v.held = nil
	out := make([]byte, 0, len(in))

	for i := 0; i < len(in); {
		if !utf8.FullRune(in[i:]) {
			v.held = append([]byte(nil), in[i:]...)

			break
		}

		r, size := utf8.DecodeRune(in[i:])
		if r == utf8.RuneError && size == 1 {
			r = rune(in[i]) // a stray byte is judged by its value
		}

		switch {
		case r == '\n' || r == '\t':
			out = append(out, in[i:i+size]...)
		case r < 0x20 || r == 0x7f:
			out = append(out, caret(r)...)
		case r >= 0x80 && r <= 0x9f:
			out = append(append(out, "M-"...), caret(r-0x80)...)
		default:
			out = append(out, in[i:i+size]...)
		}

		i += size
	}

	if _, err := v.w.Write(out); err != nil {
		return 0, err
	}

	return len(p), nil
}

// caret is the cat -v form of a code below 0x20 or DEL.
func caret(r rune) []byte {
	if r == 0x7f {
		return []byte("^?")
	}

	return []byte("^" + string(r+0x40))
}
