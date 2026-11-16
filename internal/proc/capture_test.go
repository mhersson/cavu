package proc

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tenMiB holds no digits, so the only number in a capped result is the
// dropped byte count.
var tenMiB = "first line\n" + strings.Repeat("abcdefghij\n", (10<<20)/11) + "last line\n"

// splitCapped returns what a capped result kept around its marker line and
// the byte count the marker names.
func splitCapped(t *testing.T, out string) (head, tail string, dropped int) {
	t.Helper()

	i := strings.IndexFunc(out, unicode.IsDigit)
	require.GreaterOrEqual(t, i, 0, "no dropped byte count")

	start := strings.LastIndexByte(out[:i], '\n')
	end := strings.IndexByte(out[i:], '\n')

	require.GreaterOrEqual(t, start, 0, "marker does not start a line")
	require.GreaterOrEqual(t, end, 0, "marker does not end a line")

	digits := out[i:]
	if j := strings.IndexFunc(digits, func(r rune) bool { return !unicode.IsDigit(r) }); j >= 0 {
		digits = digits[:j]
	}

	dropped, err := strconv.Atoi(digits)
	require.NoError(t, err)

	return out[:start], out[i+end+1:], dropped
}

func TestHeadTail(t *testing.T) {
	t.Parallel()

	t.Run("caps a large input", func(t *testing.T) {
		t.Parallel()

		out := HeadTail(tenMiB, CaptureCap)

		assert.LessOrEqual(t, len(out), CaptureCap)
		assert.True(t, strings.HasPrefix(out, "first line\n"))
		assert.True(t, strings.HasSuffix(out, "\nlast line\n"))

		head, tail, dropped := splitCapped(t, out)
		assert.True(t, strings.HasPrefix(tenMiB, head))
		assert.True(t, strings.HasSuffix(tenMiB, tail))
		assert.Equal(t, len(tenMiB)-len(head)-len(tail), dropped)
	})

	t.Run("input within the cap is unchanged", func(t *testing.T) {
		t.Parallel()

		for _, in := range []string{"", "short\n", strings.Repeat("x", CaptureCap)} {
			assert.Equal(t, in, HeadTail(in, CaptureCap))
		}
	})

	t.Run("never splits a multi-byte character", func(t *testing.T) {
		t.Parallel()

		for offset := range 9 {
			in := strings.Repeat("a", offset) + strings.Repeat("é€😀", 20000)
			out := HeadTail(in, CaptureCap)

			assert.LessOrEqual(t, len(out), CaptureCap)
			assert.True(t, utf8.ValidString(out), "offset %d", offset)
		}
	})

	t.Run("a cap too small for the marker keeps a prefix", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "€", HeadTail("€€€", 5))
		assert.Empty(t, HeadTail("abc", 0))
	})
}

func TestCaptureStreams(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in    string
		limit int
		chunk int
	}{
		"lines in 4 KiB chunks":      {in: tenMiB, limit: CaptureCap, chunk: 4 << 10},
		"multi-byte in 4 KiB chunks": {in: "a" + strings.Repeat("é€😀", 1<<20), limit: CaptureCap, chunk: 4 << 10},
		"one write":                  {in: tenMiB, limit: CaptureCap, chunk: len(tenMiB)},
		"within the cap":             {in: "short\n", limit: CaptureCap, chunk: 4 << 10},
		"exactly the cap":            {in: strings.Repeat("x", CaptureCap), limit: CaptureCap, chunk: 4 << 10},
		"a cap too small for marker": {in: strings.Repeat("ab€", 1000), limit: 10, chunk: 3},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := NewCapture(tc.limit)

			for chunk := range slices.Chunk([]byte(tc.in), tc.chunk) {
				n, err := c.Write(chunk)
				require.NoError(t, err)
				require.Equal(t, len(chunk), n)
				require.LessOrEqual(t, cap(c.head)+cap(c.tail), tc.limit)
			}

			assert.Equal(t, HeadTail(tc.in, tc.limit), c.String())
		})
	}
}
