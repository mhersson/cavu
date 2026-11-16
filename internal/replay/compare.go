package replay

import (
	"cmp"
	"slices"

	"github.com/mhersson/cavu/internal/store"
)

// Row pairs one source task's items from two replays; a side is nil when that replay has none.
type Row struct {
	SourceTaskID int64
	Title        string
	Now, Before  *store.ReplayItem
}

// Compare pairs the items of two replays by source task, ordered by source task id.
func Compare(now, before []store.ReplayItem, titles map[int64]string) []Row {
	rows := map[int64]*Row{}

	row := func(id int64) *Row {
		if rows[id] == nil {
			rows[id] = &Row{SourceTaskID: id, Title: titles[id]}
		}

		return rows[id]
	}

	for i := range now {
		row(now[i].SourceTaskID).Now = &now[i]
	}

	for i := range before {
		row(before[i].SourceTaskID).Before = &before[i]
	}

	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}

	slices.SortFunc(out, func(a, b Row) int { return cmp.Compare(a.SourceTaskID, b.SourceTaskID) })

	return out
}
