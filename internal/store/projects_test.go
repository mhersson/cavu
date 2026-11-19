package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fullProject() Project {
	return Project{
		Name: "demo", RepoURL: "https://example.com/demo.git", BaseBranch: "main",
		SetupCommand: "make setup", VerifyCommand: "make check", VerifyWaived: true,
		ToolOverrides: map[string]string{"go": "1.27"},
		EgressHosts:   []string{"proxy.golang.org", "example.com"},
		Env:           map[string]string{"CI": "1"},
		SecretVars:    []string{"NPM_TOKEN"},
		Switches:      map[string]bool{"continuations": true, "web_tools": false},
		Caps:          map[string]string{"task_max_turns": "50"},
	}
}

func TestProjectRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	created, err := s.CreateProject(ctx, fullProject())
	require.NoError(t, err)
	require.NotZero(t, created.ID)

	tests := []struct {
		name string
		get  func() (Project, error)
	}{
		{"by id", func() (Project, error) { return s.Project(ctx, created.ID) }},
		{"by name", func() (Project, error) { return s.ProjectByName(ctx, "demo") }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.get()
			require.NoError(t, err)
			require.Equal(t, created, got)
		})
	}

	at := time.UnixMilli(1_700_000_000_123)
	created.Name = "renamed"
	created.EgressHosts = nil
	created.Baseline = &Baseline{Commit: "abc", Passed: true, At: at, Output: "ok"}
	require.NoError(t, s.UpdateProject(ctx, created))

	got, err := s.Project(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
	require.Empty(t, got.EgressHosts)
	require.NotNil(t, got.Baseline)
	require.Equal(t, "abc", got.Baseline.Commit)
	require.True(t, got.Baseline.Passed)
	require.True(t, at.Equal(got.Baseline.At))
	require.Equal(t, "ok", got.Baseline.Output)

	all, err := s.Projects(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestProjectNameUnique(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	_, err := s.CreateProject(ctx, Project{Name: "a"})
	require.NoError(t, err)

	_, err = s.CreateProject(ctx, Project{Name: "a"})
	require.ErrorIs(t, err, ErrConflict)

	b, err := s.CreateProject(ctx, Project{Name: "b"})
	require.NoError(t, err)

	b.Name = "a"
	require.ErrorIs(t, s.UpdateProject(ctx, b), ErrConflict)
}

func TestProjectCheckFields(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	at := time.UnixMilli(1_700_000_000_456)
	p := fullProject()
	p.CheckRequestedAt = &at
	p.CheckResult = CheckResult{
		Tools: []string{"go"}, IgnoredTools: []string{"zig"}, BaseCommit: "base123",
		FailedStep: "setup", Output: "no rule", VerifySuggestion: "make check", Fault: "boom",
	}

	created, err := s.CreateProject(ctx, p)
	require.NoError(t, err)

	got, err := s.Project(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created, got)

	created.CheckRequestedAt = nil
	require.NoError(t, s.UpdateProject(ctx, created))

	got, err = s.Project(ctx, created.ID)
	require.NoError(t, err)
	require.Nil(t, got.CheckRequestedAt)
	require.Equal(t, created.CheckResult, got.CheckResult)
}

func TestProjectForgePolicyRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	p := fullProject()
	p.Forge = ForgePolicy{NoCI: true, IgnoredChecks: []string{"lint"}, ReviewBots: []string{"copilot[bot]"}}

	created, err := s.CreateProject(ctx, p)
	require.NoError(t, err)

	got, err := s.Project(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, p.Forge, got.Forge)

	created.Forge = ForgePolicy{IgnoredChecks: []string{"a", "b"}}
	require.NoError(t, s.UpdateProject(ctx, created))

	got, err = s.Project(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.Forge, got.Forge)
}

func TestRemoveProject(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	foo, err := s.CreateProject(ctx, Project{Name: "foo"})
	require.NoError(t, err)

	other, err := s.CreateProject(ctx, Project{Name: "foo-bar"})
	require.NoError(t, err)

	task, err := s.CreateTask(ctx, Task{ProjectID: foo.ID, Title: "finished"})
	require.NoError(t, err)

	for _, name := range []string{"project.foo.NPM_TOKEN", "project.foo.OTHER", "project.foo-bar.NPM_TOKEN", "forge_token", "project_foo_x"} {
		require.NoError(t, s.PutSecret(ctx, name, []byte("v")))
	}

	at := time.UnixMilli(1_700_000_000_000)
	require.NoError(t, s.RemoveProject(ctx, foo.ID, at, "project.foo."))

	got, err := s.Project(ctx, foo.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RemovedAt)
	require.True(t, at.Equal(*got.RemovedAt))
	require.Equal(t, "foo", got.Name, "a removed project keeps its name for its finished tasks")

	_, err = s.ProjectByName(ctx, "foo")
	require.ErrorIs(t, err, ErrNotFound)

	all, err := s.Projects(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, other.ID, all[0].ID)

	names, err := s.SecretNames(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"forge_token", "project.foo-bar.NPM_TOKEN", "project_foo_x"}, names,
		"only the secrets under the exact prefix go")

	kept, err := s.Task(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, foo.ID, kept.ProjectID)

	require.ErrorIs(t, s.RemoveProject(ctx, foo.ID, at, "project.foo."), ErrNotFound, "a second removal")
	require.ErrorIs(t, s.RemoveProject(ctx, 999, at, "project.x."), ErrNotFound)

	_, err = s.CreateTask(ctx, Task{ProjectID: foo.ID, Title: "late"})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.CreateTask(ctx, Task{ProjectID: 999, Title: "orphan"})
	require.ErrorIs(t, err, ErrNotFound)

	got.RepoURL = "changed"
	got.RemovedAt = nil
	require.NoError(t, s.UpdateProject(ctx, got))

	again, err := s.Project(ctx, foo.ID)
	require.NoError(t, err)
	require.NotNil(t, again.RemovedAt, "an update never brings a removed project back")

	reborn, err := s.CreateProject(ctx, Project{Name: "foo"})
	require.NoError(t, err, "the name can be used again")
	require.NotEqual(t, foo.ID, reborn.ID)

	_, err = s.CreateProject(ctx, Project{Name: "foo"})
	require.ErrorIs(t, err, ErrConflict)

	byName, err := s.ProjectByName(ctx, "foo")
	require.NoError(t, err)
	require.Equal(t, reborn.ID, byName.ID)
}

func TestRemoveProjectRollsBackWithItsSecrets(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	p, err := s.CreateProject(ctx, Project{Name: "foo"})
	require.NoError(t, err)
	require.NoError(t, s.PutSecret(ctx, "project.foo.X", []byte("v")))

	_, err = s.db.Exec(`CREATE TRIGGER no_secret_delete BEFORE DELETE ON secrets BEGIN SELECT RAISE(ABORT, 'no'); END`)
	require.NoError(t, err)

	require.Error(t, s.RemoveProject(ctx, p.ID, time.Now(), "project.foo."))

	got, err := s.Project(ctx, p.ID)
	require.NoError(t, err)
	require.Nil(t, got.RemovedAt, "the mark and the secrets go together")
}

func TestProjectNames(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	a, err := s.CreateProject(ctx, Project{Name: "a"})
	require.NoError(t, err)

	b, err := s.CreateProject(ctx, Project{Name: "b"})
	require.NoError(t, err)

	require.NoError(t, s.RemoveProject(ctx, a.ID, time.Now(), "project.a."))

	names, err := s.ProjectNames(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64]string{a.ID: "a", b.ID: "b"}, names, "a removed project's name too")
}
