package replay_test

import (
	"testing"

	"github.com/mhersson/cavu/internal/replay"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	now := []store.ReplayItem{{SourceTaskID: 3, Turns: 1}, {SourceTaskID: 1, Turns: 2}}
	before := []store.ReplayItem{{SourceTaskID: 2, Turns: 5}, {SourceTaskID: 1, Turns: 4}}
	titles := map[int64]string{1: "one", 2: "two", 3: "three"}

	rows := replay.Compare(now, before, titles)

	assert.Equal(t, []replay.Row{
		{SourceTaskID: 1, Title: "one", Now: &now[1], Before: &before[1]},
		{SourceTaskID: 2, Title: "two", Before: &before[0]},
		{SourceTaskID: 3, Title: "three", Now: &now[0]},
	}, rows)
}
