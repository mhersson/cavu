package forge

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

const pullsPath = "/api/v3/repos/o/n/pulls"

func TestOpenPROpensADraft(t *testing.T) {
	bare, _ := newBareRepo(t)
	f, c := newFake(t, bare, nil)

	pr := openPR(t, c)

	assert.Equal(t, 1, pr.Number)
	assert.Contains(t, pr.URL, "/o/n/pull/1")
	assert.Equal(t, []forgefake.Pull{{
		Number: 1, NodeID: pr.NodeID, Title: "T", Body: "B", Base: "main", Head: "cavu/x", Draft: true, State: "open",
	}}, f.Pulls())
}

func TestOpenPRFindsTheBranchsPullRequest(t *testing.T) {
	bare, _ := newBareRepo(t)
	f, c := newFake(t, bare, nil)

	other, err := c.OpenPR(t.Context(), fakeRepo, fakeAccess, NewPR{Title: "Y", Base: "main", Head: "cavu/y"})
	require.NoError(t, err)

	first := openPR(t, c)
	assert.NotEqual(t, other.Number, first.Number, "another branch's pull request")

	again := openPR(t, c)
	assert.Equal(t, first, again)
	assert.Len(t, f.Pulls(), 2)
	assert.Equal(t, 2, sent(f, http.MethodPost, pullsPath))

	f.Close(first.Number)

	reopened := openPR(t, c)
	assert.Equal(t, 3, reopened.Number, "a closed pull request is not the branch's")
}

func TestOpenPRWithoutDrafts(t *testing.T) {
	bare, _ := newBareRepo(t)

	t.Run("opens one that is ready", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.NoDrafts()

		pr := openPR(t, c)

		pulls := f.Pulls()
		require.Len(t, pulls, 1)
		assert.Equal(t, pr.NodeID, pulls[0].NodeID)
		assert.False(t, pulls[0].Draft)
		assert.Equal(t, 2, sent(f, http.MethodPost, pullsPath))
	})

	t.Run("another refusal is returned", func(t *testing.T) {
		f, c := newFake(t, bare, nil)

		_, err := c.OpenPR(t.Context(), fakeRepo, fakeAccess, NewPR{Title: "T", Base: "main", Head: "cavu/missing"})

		he, ok := errors.AsType[*httpError](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, http.StatusUnprocessableEntity, he.status)
		assert.Empty(t, f.Pulls())
		assert.Equal(t, 2, sent(f, http.MethodPost, pullsPath))
	})
}

func TestReadyTwice(t *testing.T) {
	bare, _ := newBareRepo(t)
	f, c := newFake(t, bare, nil)
	pr := openPR(t, c)

	require.NoError(t, c.Ready(t.Context(), fakeRepo, fakeAccess, pr.NodeID))
	assert.False(t, f.Pulls()[0].Draft)

	require.NoError(t, c.Ready(t.Context(), fakeRepo, fakeAccess, pr.NodeID))
	assert.Equal(t, []string{"Draft", "Ready", "Draft"}, operations(f), "a ready one is only asked")
}

func TestObserveStates(t *testing.T) {
	bare, head := newBareRepo(t)

	tests := []struct {
		name     string
		script   func(f *forgefake.Fake)
		state    string
		conflict bool
	}{
		{"open", func(f *forgefake.Fake) { f.Conflict(1, false) }, "open", false},
		{"merged", func(f *forgefake.Fake) { f.Merge(1) }, "merged", false},
		{"closed", func(f *forgefake.Fake) { f.Close(1) }, "closed", false},
		{"a conflict", func(f *forgefake.Fake) { f.Conflict(1, true) }, "open", true},
		{"not yet known", func(*forgefake.Fake) {}, "open", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t, bare, nil)
			openPR(t, c)
			tc.script(f)

			o, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
			require.NoError(t, err)
			assert.Equal(t, tc.state, o.State)
			assert.Equal(t, tc.conflict, o.Conflict)
			assert.True(t, o.Draft)
			assert.Equal(t, head, o.Head)
		})
	}
}

func TestObserveReportsAForgeError(t *testing.T) {
	bare, _ := newBareRepo(t)
	f, c := newFake(t, bare, nil)
	openPR(t, c)

	f.Fail(1)

	_, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
	he, ok := errors.AsType[*httpError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, http.StatusBadGateway, he.status)

	f.Limit(testClock)

	_, err = c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1})
	require.ErrorIs(t, err, ErrRateLimited)
}
