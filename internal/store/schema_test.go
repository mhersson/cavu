package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEventsAreAppendOnly(t *testing.T) {
	t.Parallel()

	s := openTest(t)
	task := seedTask(t, s)

	_, err := s.Append(context.Background(), task.ID, -1, []Event{{Type: "x", Actor: "user", Payload: []byte(`{}`)}}, nil)
	require.NoError(t, err)

	for _, stmt := range []string{
		`UPDATE events SET type = 'y'`,
		`DELETE FROM events`,
	} {
		_, err := s.db.Exec(stmt)
		require.Error(t, err, stmt)
	}

	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestForeignKeysEnforced(t *testing.T) {
	t.Parallel()

	s := openTest(t)

	_, err := s.CreateTask(context.Background(), Task{ProjectID: 999, Title: "orphan"})
	require.Error(t, err)
}
