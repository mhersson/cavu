package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openTest(t *testing.T) *Store {
	t.Helper()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "cavu.db"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func seedTask(t *testing.T, s *Store) Task {
	t.Helper()

	ctx := context.Background()

	p, err := s.CreateProject(ctx, Project{Name: "p" + t.Name(), RepoURL: "r", BaseBranch: "main"})
	require.NoError(t, err)

	task, err := s.CreateTask(ctx, Task{ProjectID: p.ID, Title: "t"})
	require.NoError(t, err)

	return task
}

func TestOpenCreatesPrivateParentDir(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "data", "cavu.db")

	s, err := Open(context.Background(), path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	info, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestNotFound(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"Project", func() error {
			_, err := s.Project(ctx, 1)

			return err
		}},
		{"ProjectByName", func() error {
			_, err := s.ProjectByName(ctx, "x")

			return err
		}},
		{"Task", func() error {
			_, err := s.Task(ctx, 1)

			return err
		}},
		{"UpdateProject", func() error { return s.UpdateProject(ctx, Project{ID: 1, Name: "x"}) }},
		{"UpdateTask", func() error { return s.UpdateTask(ctx, Task{ID: 1}) }},
		{"EditTask", func() error { return s.EditTask(ctx, 1, TaskEdit{Title: new("x")}, -1) }},
		{"UpdateRun", func() error { return s.UpdateRun(ctx, Run{ID: 1}) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.call(), ErrNotFound)
		})
	}
}
