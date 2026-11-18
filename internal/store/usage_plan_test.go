package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDayUsageUsesItsIndex(t *testing.T) {
	t.Parallel()

	rows, err := openTest(t).db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+usageBetweenQuery, 0, 1)
	require.NoError(t, err)

	defer rows.Close()

	var plan []string

	for rows.Next() {
		var id, parent, unused int

		var detail string

		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))

		plan = append(plan, detail)
	}

	require.NoError(t, rows.Err())

	for _, step := range plan {
		require.False(t, strings.HasPrefix(step, "SCAN "), "full scan in plan: %v", plan)
	}
}
