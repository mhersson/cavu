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
