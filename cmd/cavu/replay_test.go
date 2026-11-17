package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markWithBase marks the task for replay and gives it a base commit.
func markWithBase(t *testing.T, dir, id string) {
	t.Helper()

	cavuIn(t, dir, "task", "mark", id)

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	n, err := strconv.ParseInt(id, 10, 64)
	require.NoError(t, err)

	task, err := a.store.Task(t.Context(), n)
	require.NoError(t, err)

	task.BaseCommit = "base123"
	require.NoError(t, a.store.UpdateTask(t.Context(), task))
}

// storedReplays returns every replay with its items, oldest first.
func storedReplays(t *testing.T, dir string) (replays []store.Replay, items map[int64][]store.ReplayItem) {
	t.Helper()

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	items = map[int64][]store.ReplayItem{}

	for id := int64(1); ; id++ {
		r, ok, err := a.store.PreviousReplay(t.Context(), id+1)
		require.NoError(t, err)

		if !ok || r.ID != id {
			return replays, items
		}

		replays = append(replays, r)
		items[id], err = a.store.ReplayItems(t.Context(), id)
		require.NoError(t, err)
	}
}

func TestReplayVerbEmptySet(t *testing.T) {
	dir, _, fake := runSetup(t)

	code, stdout, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)

	assert.Equal(t, 0, code, stderr)
	assert.Contains(t, stdout, "cavu task mark")
	assert.Empty(t, fake.runner.actions, "no container starts")

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays)
}

func TestReplayVerbRequiresName(t *testing.T) {
	dir, _, fake := runSetup(t)

	code, _, _ := runCavu("replay", "--data-dir", dir)

	assert.Equal(t, 2, code)
	assert.Empty(t, fake.removals)
}

func TestReplayVerbPrintsOneRowPerSource(t *testing.T) {
	dir, a, fake := runSetup(t)
	b := addTask(t, dir, "Fix the logout")
	markWithBase(t, dir, a)
	markWithBase(t, dir, b)

	// Header, then one line per source task; the previous columns are the
	// ones that hold the first replay's values.
	table := func(stdout string) [][]string {
		var rows [][]string

		for l := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
			if f := strings.Fields(l); len(f) > 0 && (f[0] == a || f[0] == b) {
				rows = append(rows, f)
			}
		}

		return rows
	}

	code, stdout, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)
	assert.Len(t, fake.removals, 1)

	_, items := storedReplays(t, dir)
	require.Len(t, items[1], 2)

	first := table(stdout)
	require.Len(t, first, 2)

	code, stdout, stderr = runCavu("replay", "--name", "second", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	second := table(stdout)
	require.Len(t, second, 2)
	assert.Greater(t, len(second[0]), len(first[0]), "the previous replay's values fill the columns beside this one's")

	_, items = storedReplays(t, dir)
	assert.Equal(t, items[2][0].DiffPath, second[0][len(second[0])-1], "the diff path is this replay's")
}

func TestReplayVerbFlagsReachConfig(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	code, _, stderr := runCavu("replay", "--name", "tuned", "--model", "model-b", "--effort", "high",
		"--no-continuations", "--no-verify-fix", "--image", "cavu-sandbox:other", "--data-dir", dir)
	require.Equal(t, 0, code, stderr)

	replays, _ := storedReplays(t, dir)
	require.Len(t, replays, 1)
	assert.Equal(t, "tuned", replays[0].Name)
	assert.Equal(t, "cavu-sandbox:other", replays[0].Image)
	assert.Equal(t, map[string]string{
		"model_work": "model-b", "effort_work": "high", "continuations": "false", "verify_fix": "false",
	}, replays[0].Config)
	assert.Empty(t, fake.image, "an explicit image is not checked")
}

func TestReplayVerbChecksTheDefaultImage(t *testing.T) {
	dir, id, fake := runSetup(t)
	markWithBase(t, dir, id)

	fake.labels.CavuVersion = "0.0.1"

	code, _, stderr := runCavu("replay", "--name", "first", "--data-dir", dir)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "cavu image build")
	assert.Equal(t, sandbox.ImageName(version), fake.image)
	assert.Empty(t, fake.runner.actions)

	replays, _ := storedReplays(t, dir)
	assert.Empty(t, replays)
}
