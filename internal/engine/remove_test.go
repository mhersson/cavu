package engine_test

import (
	"errors"
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secrets seeds one project secret of web, one of web-2 and a secret of the instance.
func (h *harness) secrets() {
	h.t.Helper()

	for _, name := range []string{engine.ProjectSecretName("web", "NPM_TOKEN"), engine.ProjectSecretName("web-2", "NPM_TOKEN"), engine.SecretForgeToken} {
		require.NoError(h.t, h.vault.Set(h.t.Context(), name, "v"))
	}
}

func (h *harness) secretNames() []string {
	h.t.Helper()

	names, err := h.store.SecretNames(h.t.Context())
	require.NoError(h.t, err)

	return names
}

func (h *harness) listed() []string {
	h.t.Helper()

	ps, err := h.store.Projects(h.t.Context())
	require.NoError(h.t, err)

	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}

	return names
}

func TestRemoveProjectIsRefusedWhileATaskIsOpen(t *testing.T) {
	// shadow starts a replay's shadow task of web, which Cancel clears like any other.
	shadow := func(h *harness) int64 {
		t.Helper()

		s, err := h.store.CreateTask(t.Context(), store.Task{ProjectID: h.task.ProjectID, Title: "shadow", ReplayID: 1})
		require.NoError(t, err)

		_, err = h.store.Append(t.Context(), s.ID, -1, []store.Event{{Type: engine.TaskStarted, Actor: "user", Payload: []byte("{}")}}, nil)
		require.NoError(t, err)

		return s.ID
	}

	t.Run("a task", func(t *testing.T) {
		h := newHarness(t)
		h.secrets()

		require.ErrorIs(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID), engine.ErrRefused)
		assert.Empty(t, h.runner.callsOf("remove-project"))
		assert.Equal(t, []string{"web"}, h.listed())
		assert.Len(t, h.secretNames(), 3)

		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))
		require.NoError(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID))
	})

	t.Run("a replay's shadow", func(t *testing.T) {
		h := newHarness(t)
		h.secrets()
		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

		id := shadow(h)

		require.ErrorIs(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID), engine.ErrRefused)
		assert.Empty(t, h.runner.callsOf("remove-project"))
		assert.Equal(t, []string{"web"}, h.listed())
		assert.Len(t, h.secretNames(), 3)

		require.NoError(t, h.loop.Cancel(t.Context(), id), "the action the refusal names clears a shadow")
		require.NoError(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID))
	})
}

func TestRemoveProject(t *testing.T) {
	t.Run("a project whose tasks are over", func(t *testing.T) {
		h := newHarness(t)
		h.secrets()

		// A task of another project that is still open is no reason to refuse.
		other, err := h.store.CreateProject(t.Context(), store.Project{Name: "api", RepoURL: "https://example.com/api.git", BaseBranch: "main"})
		require.NoError(t, err)
		_, err = h.store.CreateTask(t.Context(), store.Task{ProjectID: other.ID, Title: "open"})
		require.NoError(t, err)

		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

		require.NoError(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID))

		calls := h.runner.callsOf("remove-project")
		require.Len(t, calls, 1)
		assert.Equal(t, engine.RunRef{InstanceID: "inst-1", Image: "cavu-sandbox:test", ProjectID: h.task.ProjectID}, calls[0].ref)
		assert.Equal(t, []string{"api"}, h.listed())
		assert.ElementsMatch(t, []string{engine.ProjectSecretName("web-2", "NPM_TOKEN"), engine.SecretForgeToken}, h.secretNames())

		p, err := h.store.Project(t.Context(), h.task.ProjectID)
		require.NoError(t, err)
		assert.True(t, h.clock.Now().Equal(*p.RemovedAt))
	})

	t.Run("a failed volume removal changes nothing", func(t *testing.T) {
		h := newHarness(t)
		h.secrets()
		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))

		h.runner.removeErr = errors.New("volume is in use")

		err := h.loop.RemoveProject(t.Context(), h.task.ProjectID)
		require.ErrorIs(t, err, engine.ErrVolumesNotRemoved)
		require.NotErrorIs(t, err, engine.ErrRefused)
		assert.Contains(t, err.Error(), "volume is in use")
		assert.Equal(t, []string{"web"}, h.listed())
		assert.Len(t, h.secretNames(), 3)
	})

	t.Run("a removed or unknown project", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.loop.Cancel(t.Context(), h.task.ID))
		require.NoError(t, h.loop.RemoveProject(t.Context(), h.task.ProjectID))
		h.runner.mu.Lock()
		h.runner.calls = nil
		h.runner.mu.Unlock()

		for _, id := range []int64{h.task.ProjectID, 999} {
			require.ErrorIs(t, h.loop.RemoveProject(t.Context(), id), store.ErrNotFound)
		}

		assert.Empty(t, h.runner.callsOf("remove-project"))
	})
}
