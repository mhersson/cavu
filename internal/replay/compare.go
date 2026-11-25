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

// Compare pairs outcomes only when the source task and the inputs its shadows ran with match.
func Compare(now, before []store.ReplayItem) []Row {
	rows := map[int64]*Row{}

	for i := range now {
		n := &now[i]
		rows[n.SourceTaskID] = &Row{SourceTaskID: n.SourceTaskID, Title: n.Inputs.Title, Now: n}
	}

	for i := range before {
		b := &before[i]
		if r := rows[b.SourceTaskID]; r == nil {
			rows[b.SourceTaskID] = &Row{SourceTaskID: b.SourceTaskID, Title: b.Inputs.Title, Before: b}
		} else if r.Now.Inputs == b.Inputs {
			r.Before = b
		}
	}

	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}

	slices.SortFunc(out, func(a, b Row) int { return cmp.Compare(a.SourceTaskID, b.SourceTaskID) })

	return out
}
