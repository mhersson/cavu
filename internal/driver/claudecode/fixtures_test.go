package claudecode

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/driver"
)

// credentialPattern matches an Anthropic key, an OAuth token marker or an env
// dump naming the token variable.
var credentialPattern = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}|oat01|CLAUDE_CODE_OAUTH_TOKEN=`)

func TestCredentialPatternCatchesTokenShapes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"api key", "x-api-key: sk-ant-" + "api03-Ab1_cD2-eF3", true},
		{"oauth token marker", `"token":"sk-` + `ant-oat01-Zz9"`, true},
		{"env dump", "CLAUDE_CODE_OAUTH_TOKEN=" + "anything", true},
		{"probe dummy bearer", "authorization: Bearer dummy-oauth", false},
		{"short prefix", "sk-ant-abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, credentialPattern.MatchString(tt.in))
		})
	}
}

// TestFixturesCarryNoToken fails if any fixture looks like a real credential.
func TestFixturesCarryNoToken(t *testing.T) {
	root, err := os.OpenRoot("testdata")
	require.NoError(t, err)

	t.Cleanup(func() { _ = root.Close() })

	fsys := root.FS()

	var files int

	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		files++

		assert.Falsef(t, credentialPattern.Match(data), "testdata/%s matches the credential pattern", path)

		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, files, "testdata holds no files")
}

// TestMapperEveryFixture replays every recording through the mapper, so each
// copied fixture is exercised, and a new one needs an expected reason here.
func TestMapperEveryFixture(t *testing.T) {
	want := map[string]string{
		"real/dl-httphook.jsonl":              driver.ReasonDone,
		"real/p3.jsonl":                       driver.ReasonDone,
		"real/p3b.jsonl":                      driver.ReasonDone,
		"real/p4-partial-messages.jsonl":      driver.ReasonDone,
		"real/t1.jsonl":                       driver.ReasonDone,
		"real/t2.jsonl":                       driver.ReasonAuth,
		"real/t3-iso.jsonl":                   driver.ReasonDone,
		"real/t5.jsonl":                       driver.ReasonDone,
		"real/t6.jsonl":                       driver.ReasonDone,
		"real/t7.jsonl":                       driver.ReasonDone,
		"real/t7b.jsonl":                      driver.ReasonDone,
		"synthetic/crash.jsonl":               driver.ReasonCrashed,
		"synthetic/max_budget_end_turn.jsonl": driver.ReasonMaxBudget,
		"synthetic/max_budget_tool_use.jsonl": driver.ReasonMaxBudget,
		"synthetic/max_turns.jsonl":           driver.ReasonMaxTurns,
		"synthetic/parallel.jsonl":            driver.ReasonDone,
		"synthetic/partial.jsonl":             driver.ReasonDone,
		"synthetic/rejected.jsonl":            driver.ReasonUsageLimit,
		"synthetic/two_models.jsonl":          driver.ReasonDone,
		"synthetic/unknown.jsonl":             driver.ReasonDone,
	}

	var found []string

	for _, dir := range []string{"real", "synthetic"} {
		entries, err := os.ReadDir(filepath.Join("testdata", dir))
		require.NoError(t, err)

		for _, e := range entries {
			found = append(found, dir+"/"+e.Name())
		}
	}

	assert.ElementsMatch(t, slices.Collect(maps.Keys(want)), found, "every fixture has an expected reason")

	for _, path := range found {
		t.Run(path, func(t *testing.T) {
			reason, ok := want[path]
			require.True(t, ok)

			evs, res := replay(t, path, 1)
			assert.NotEmpty(t, evs)
			assert.Equal(t, reason, res.Reason)
			assert.Equal(t, reason != driver.ReasonDone, res.IsError)
		})
	}
}
