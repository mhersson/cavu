// Package replay re-runs the reference tasks under a named
// configuration and records each outcome, so two replays can be compared.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// ErrNoReferenceTasks is returned when no task is marked for replay with a base commit.
var ErrNoReferenceTasks = errors.New("replay: no reference task has a base commit")

// Config names a replay and what it changes. Empty and nil fields change nothing.
type Config struct {
	Name, Model, Effort                string
	Continuations, VerifyFix, WebTools *bool
}

// overrides maps c onto the task override keys the engine understands.
func (c Config) overrides() map[string]string {
	o := map[string]string{}

	if c.Model != "" {
		o["model_work"] = c.Model
	}

	if c.Effort != "" {
		o["effort_work"] = c.Effort
	}

	for k, v := range map[string]*bool{"continuations": c.Continuations, "verify_fix": c.VerifyFix, "web_tools": c.WebTools} {
		if v != nil {
			o[k] = strconv.FormatBool(*v)
		}
	}

	return o
}

// Run runs a shadow of every reference task under c, one after another, and
// records an item for each. A stop is an item's outcome; any error, from RunTask
// or the store, ends the replay, which then stays unended and is never a comparison base.
func Run(ctx context.Context, s *store.Store, l *engine.Loop, c Config, out io.Writer) (store.Replay, error) {
	all, err := s.Tasks(ctx, false)
	if err != nil {
		return store.Replay{}, fmt.Errorf("replay: %w", err)
	}

	var sources []store.Task

	for _, t := range all {
		if t.Reference && t.BaseCommit != "" {
			sources = append(sources, t)
		}
	}

	if len(sources) == 0 {
		return store.Replay{}, ErrNoReferenceTasks
	}

	r, err := s.CreateReplay(ctx, store.Replay{Name: c.Name, Image: l.Image, Config: c.overrides()})
	if err != nil {
		return store.Replay{}, fmt.Errorf("replay: %w", err)
	}

	for _, src := range sources {
		item, err := runOne(ctx, s, l, r, src)
		if err != nil {
			return r, err
		}

		if err := s.PutReplayItem(ctx, item); err != nil {
			return r, fmt.Errorf("replay: %w", err)
		}

		fmt.Fprintf(out, "task %d (%q): verify %q, stop %q, %d turns, %d tokens\n",
			src.ID, src.Title, item.Verify, item.Stop, item.Turns, item.Tokens)
	}

	at := l.Now()
	if err := s.EndReplay(ctx, r.ID, at); err != nil {
		return r, fmt.Errorf("replay: %w", err)
	}

	r.EndedAt = &at

	return r, nil
}

func runOne(ctx context.Context, s *store.Store, l *engine.Loop, r store.Replay, src store.Task) (store.ReplayItem, error) {
	shadow, err := s.CreateTask(ctx, store.Task{
		ProjectID: src.ProjectID, Title: src.Title, Body: src.Body, Requester: src.Requester,
		BaseCommit: src.BaseCommit, ReplayID: r.ID, Overrides: maps.Clone(r.Config),
	})
	if err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: shadow of task %d: %w", src.ID, err)
	}

	start := store.Event{At: l.Now(), Type: engine.TaskStarted, Actor: "user", Payload: json.RawMessage("{}")}

	if _, err := s.Append(ctx, shadow.ID, -1, []store.Event{start}, nil); err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: start shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	status, err := l.RunTask(ctx, shadow.ID)
	if err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: run shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	item, err := record(ctx, s, shadow)
	if err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: record shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	// Nothing resumes a shadow, so once its stop is recorded a cancel ends its run and removes its volumes.
	if status == engine.StatusBlocked {
		if err = l.Cancel(ctx, shadow.ID); err == nil {
			_, err = l.RunTask(ctx, shadow.ID)
		}

		if err != nil {
			return store.ReplayItem{}, fmt.Errorf("replay: cancel shadow %d of task %d: %w", shadow.ID, src.ID, err)
		}
	}

	item.ReplayID, item.SourceTaskID = r.ID, src.ID

	return item, nil
}

// record derives the item from the shadow's events and usage.
func record(ctx context.Context, s *store.Store, shadow store.Task) (store.ReplayItem, error) {
	events, err := s.Events(ctx, shadow.ID, 0, 0)
	if err != nil {
		return store.ReplayItem{}, err
	}

	usage, err := s.UsageTotals(ctx, shadow.ID)
	if err != nil {
		return store.ReplayItem{}, err
	}

	item := store.ReplayItem{TaskID: shadow.ID}

	if stop := engine.Reduce(events).OpenStop; stop != nil {
		item.Stop = stop.Reason
	}

	var started time.Time

	for _, e := range events {
		switch e.Type {
		case engine.TaskStarted:
			started = e.At
		case engine.VerifyEnded:
			var p engine.VerifyEndedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return store.ReplayItem{}, fmt.Errorf("event %d: %w", e.ID, err)
			}

			item.Verify = p.Result
		case engine.SessionEnded:
			var p engine.SessionEndedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return store.ReplayItem{}, fmt.Errorf("event %d: %w", e.ID, err)
			}

			item.Turns += p.Turns
			item.Asked = item.Asked || p.Outcome == engine.OutcomeBlocked
		case engine.Exported:
			var p engine.ExportedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return store.ReplayItem{}, fmt.Errorf("event %d: %w", e.ID, err)
			}

			item.DiffPath = p.DiffPath
		}
	}

	if len(events) > 0 {
		item.Duration = events[len(events)-1].At.Sub(started)
	}

	for _, u := range usage {
		item.Tokens += u.Input + u.Output + u.CacheRead + u.CacheWrite
		item.CostEstimateUSD += u.CostEstimateUSD
	}

	return item, nil
}
