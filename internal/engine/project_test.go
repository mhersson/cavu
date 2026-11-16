package engine_test

import (
	"testing"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withBaseline stores a passed baseline on the harness's project and returns the project.
func (h *harness) withBaseline() store.Project {
	h.t.Helper()

	p, err := h.store.Project(h.t.Context(), h.task.ProjectID)
	require.NoError(h.t, err)

	p.Baseline = &store.Baseline{Commit: "old999", Passed: true, At: now, Output: "ok"}
	require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))

	return p
}

func (h *harness) project() store.Project {
	h.t.Helper()

	p, err := h.store.Project(h.t.Context(), h.task.ProjectID)
	require.NoError(h.t, err)

	return p
}

func TestEditProjectClearsBaseline(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(p *store.Project)
		keeps bool
	}{
		{"repository", func(p *store.Project) { p.RepoURL = "https://example.com/other.git" }, false},
		{"base branch", func(p *store.Project) { p.BaseBranch = "develop" }, false},
		{"setup command", func(p *store.Project) { p.SetupCommand = "make deps" }, false},
		{"verify command", func(p *store.Project) { p.VerifyCommand = "make check" }, false},
		{"waiver", func(p *store.Project) { p.VerifyCommand, p.VerifyWaived = "", true }, false},
		{"tool overrides", func(p *store.Project) { p.ToolOverrides = map[string]string{"go": "1.27"} }, false},
		{"environment", func(p *store.Project) { p.Env = map[string]string{"CI": "1"} }, false},
		{"secret variables", func(p *store.Project) { p.SecretVars = []string{"NPM_TOKEN"} }, false},
		{"egress hosts alone", func(p *store.Project) { p.EgressHosts = []string{"proxy.golang.org"} }, true},
		{"switches alone", func(p *store.Project) { p.Switches = map[string]bool{"web_tools": true} }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			stored := h.withBaseline().Baseline

			edit := h.project()
			edit.Baseline = nil // an edit keeps or drops the stored baseline, whatever it carries
			tc.edit(&edit)

			require.NoError(t, engine.EditProject(t.Context(), h.store, edit))

			got := h.project()
			if tc.keeps {
				require.NotNil(t, got.Baseline)
				assert.Equal(t, stored.Commit, got.Baseline.Commit)
				assert.True(t, got.Baseline.Passed)
			} else {
				assert.Nil(t, got.Baseline)
			}

			got.Baseline = nil
			assert.Equal(t, edit, got, "the edit is written")
		})
	}
}

func TestEditProjectOfAMissingProject(t *testing.T) {
	h := newHarness(t)

	err := engine.EditProject(t.Context(), h.store, store.Project{ID: 999, Name: "gone"})
	assert.ErrorIs(t, err, store.ErrNotFound)
}
