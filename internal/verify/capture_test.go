package verify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArgv(t *testing.T) {
	assert.Equal(t, []string{"bash", "-c", "set -o pipefail; make test | tee x"}, Argv("make test | tee x"))
}

func noise(n int) string {
	return strings.Repeat("noise line that is not interesting\n", n)
}

const excerptBound = 4000 + 1500 + 100

func TestExcerpt(t *testing.T) {
	t.Run("failure in the middle of a large output", func(t *testing.T) {
		out := noise(3000) + "--- FAIL: TestThing (0.00s)\n    thing_test.go:9: boom\n" + noise(3000) + "final line"
		require.Greater(t, len(out), 200<<10)

		got := Excerpt(out)
		assert.Contains(t, got, "--- FAIL: TestThing")
		assert.Contains(t, got, "thing_test.go:9: boom")
		assert.True(t, strings.HasSuffix(got, "final line"))
		assert.LessOrEqual(t, len(got), excerptBound)
	})

	t.Run("no signal gives the tail", func(t *testing.T) {
		got := Excerpt(noise(500) + "last")
		assert.LessOrEqual(t, len(got), 4000)
		assert.True(t, strings.HasSuffix(got, "last"))
	})

	t.Run("short output without signal is whole", func(t *testing.T) {
		assert.Equal(t, "all good\n", Excerpt("all good\n"))
	})

	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"maven error block", "[INFO] x\n[ERROR] Failures:\n[ERROR]   FooTest.bar:4 expected 1\n[INFO] y\n", []string{"[ERROR] Failures:", "[ERROR]   FooTest.bar:4 expected 1"}},
		{"jvm summary with failures", "Tests run: 3, Failures: 1, Errors: 0\n", []string{"Tests run: 3, Failures: 1, Errors: 0"}},
		{"jvm summary with errors", "Tests run: 3, Failures: 0, Errors: 2\n", []string{"Tests run: 3, Failures: 0, Errors: 2"}},
		{"rust panic", "running 1 test\n  thread 'a::b' panicked at src/lib.rs:3\n", []string{"thread 'a::b' panicked at src/lib.rs:3"}},
		{"python traceback", "Traceback (most recent call last):\nAssertionError: x\n", []string{"Traceback", "AssertionError: x"}},
		{"fail block window", "--- FAIL: TestA\n  detail one\n  detail two\n--- PASS: TestB\nok pkg\n", []string{"detail one", "detail two"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Excerpt(tt.out)
			for _, w := range tt.want {
				assert.Contains(t, got, w)
			}
		})
	}

	t.Run("green jvm summary is no signal", func(t *testing.T) {
		out := "Tests run: 3, Failures: 0, Errors: 0\n" + noise(300) + "end"
		assert.NotContains(t, Excerpt(out), "Tests run: 3")
	})

	t.Run("fail block ends at the next result", func(t *testing.T) {
		out := "--- FAIL: TestA\n  detail\n--- PASS: TestB\n  stray\n" + noise(300) + "end"
		got := Excerpt(out)
		assert.Contains(t, got, "detail")
		assert.NotContains(t, got, "stray")
	})

	t.Run("multi-byte markers at the cut stay valid", func(t *testing.T) {
		out := strings.Repeat("✗ failed case number one two three\n", 400) + strings.Repeat("é", 3000)
		got := Excerpt(out)
		assert.True(t, utf8.ValidString(got))
		assert.Contains(t, got, "✗ failed case")
	})

	t.Run("tail cut inside a multi-byte rune stays valid", func(t *testing.T) {
		assert.True(t, utf8.ValidString(Excerpt("FAIL x\n"+strings.Repeat("é", 3000))))
		assert.True(t, utf8.ValidString(Excerpt(strings.Repeat("€", 3000))))
	})

	t.Run("first matches are kept under the cap", func(t *testing.T) {
		got := Excerpt("FAIL first\n" + strings.Repeat("FAIL later failure line\n", 1000))
		assert.True(t, strings.HasPrefix(got, "FAIL first\n"))
		assert.LessOrEqual(t, len(got), excerptBound)
	})
}
