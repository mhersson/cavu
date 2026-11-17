package engine

import (
	"context"
	"fmt"
)

// Pauses holds every task's open stop as the store last showed it, so a reader
// outside a scheduler gives Decide the pauses a scheduler would. The zero value is ready.
type Pauses struct {
	last  map[int64]int64 // a task's newest event id when its stop was read
	stops map[int64]StopFact
}

// Read re-reads the open stop of each task whose newest event changed since the last Read.
func (p *Pauses) Read(ctx context.Context, s Store) error {
	ids, err := s.LastEventIDs(ctx)
	if err != nil {
		return err
	}

	if p.last == nil {
		p.last, p.stops = map[int64]int64{}, map[int64]StopFact{}
	}

	for id, last := range ids {
		if p.last[id] == last {
			continue
		}

		events, err := s.EventsOmitting(ctx, id, ProgressTypes...)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		if st := Reduce(events).OpenStop; st != nil {
			p.stops[id] = *st
		} else {
			delete(p.stops, id)
		}

		p.last[id] = last
	}

	return nil
}

// Of is what the other tasks' open stops mean for taskID; it fits Loop.Paused.
func (p *Pauses) Of(taskID int64) Pause { return pauseOf(p.stops, taskID) }

// pauseOf is what the open stops of tasks other than taskID mean for it: the
// lowest task with an endpoint stop, and the latest usage-limit reset.
func pauseOf(stops map[int64]StopFact, taskID int64) Pause {
	var p Pause

	for id, st := range stops {
		switch {
		case id == taskID:
		case st.Reason == StopEndpoint && (p.EndpointTask == 0 || id < p.EndpointTask):
			p.EndpointTask = id
		case st.Reason == StopUsageLimit && st.ResetsAt.After(p.LimitedUntil):
			p.LimitedUntil = st.ResetsAt
		}
	}

	return p
}
