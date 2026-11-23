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
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// ErrNoReferenceTasks is returned when no task is marked for replay with a base commit.
var ErrNoReferenceTasks = errors.New("replay: no reference task has a base commit")

// Config names a replay and what it changes. Empty and nil fields change nothing.
type Config struct {
	Name, Model, Effort                                   string
	Continuations, VerifyFix, WebTools, Readiness, Review *bool
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

	for k, v := range map[string]*bool{
		"continuations": c.Continuations, "verify_fix": c.VerifyFix, "web_tools": c.WebTools, "readiness": c.Readiness,
		"review": c.Review,
	} {
		if v != nil {
			o[k] = strconv.FormatBool(*v)
		}
	}

	return o
}

// Run runs a shadow of every reference task under c, one after another. drive
// blocks until the shadow rests and returns its status. A shadow starts as any
// task does, and a stop is its item's outcome. Any error ends the replay, which
// then stays unended and is never a comparison base.
func Run(ctx context.Context, s *store.Store, l *engine.Loop, c Config, drive func(ctx context.Context, taskID int64) (engine.Status, error), out io.Writer) (store.Replay, error) {
	all, err := s.Tasks(ctx, true)
	if err != nil {
		return store.Replay{}, fmt.Errorf("replay: %w", err)
	}

	projects, err := s.Projects(ctx)
	if err != nil {
		return store.Replay{}, fmt.Errorf("replay: %w", err)
	}

	live := make(map[int64]bool, len(projects))
	for _, p := range projects {
		live[p.ID] = true
	}

	var sources, shadows []store.Task

	for _, t := range all {
		switch {
		case t.Reference && t.BaseCommit != "" && live[t.ProjectID]:
			sources = append(sources, t)
		case t.ReplayID != 0:
			shadows = append(shadows, t)
		}
	}

	if len(sources) == 0 {
		return store.Replay{}, ErrNoReferenceTasks
	}

	if err := cancelUnfinished(ctx, s, l, shadows); err != nil {
		return store.Replay{}, err
	}

	r, err := s.CreateReplay(ctx, store.Replay{Name: c.Name, Image: l.Image, Config: c.overrides()})
	if err != nil {
		return store.Replay{}, fmt.Errorf("replay: %w", err)
	}

	for _, src := range sources {
		item, err := runOne(ctx, s, l, r, src, drive)
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

// cancelUnfinished cancels the shadows that replays which never ended left
// before they rested. Nothing resumes a shadow, and the cancel lets whoever
// drives it end its run.
func cancelUnfinished(ctx context.Context, s *store.Store, l *engine.Loop, shadows []store.Task) error {
	for _, t := range shadows {
		events, err := s.EventsOmitting(ctx, t.ID, engine.ProgressTypes...)
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}

		switch engine.StatusOf(engine.Reduce(events)) {
		case engine.StatusQueued, engine.StatusRunning, engine.StatusBlocked:
			if err := l.Cancel(ctx, t.ID); err != nil {
				return fmt.Errorf("replay: cancel shadow %d: %w", t.ID, err)
			}
		}
	}

	return nil
}

func runOne(ctx context.Context, s *store.Store, l *engine.Loop, r store.Replay, src store.Task, drive func(context.Context, int64) (engine.Status, error)) (store.ReplayItem, error) {
	shadow, err := s.CreateTask(ctx, store.Task{
		ProjectID: src.ProjectID, Title: src.Title, Body: src.Body, Requester: src.Requester,
		BaseCommit: src.BaseCommit, ReplayID: r.ID, Overrides: maps.Clone(r.Config),
	})
	if err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: shadow of task %d: %w", src.ID, err)
	}

	if err := s.CopyImages(ctx, src.ID, shadow.ID); err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	if err := l.Start(ctx, shadow.ID); err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: start shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	status, err := drive(ctx, shadow.ID)
	if err == nil && status == engine.StatusBlocked {
		// Once is enough: the agent suggests answers only in the readiness turn, which runs once per run.
		var answered bool
		if answered, err = answerSuggested(ctx, l, shadow.ID); answered {
			status, err = drive(ctx, shadow.ID)
		}
	}

	if err != nil {
		// The replay ends here, so nothing resumes the shadow: the cancel lets whoever drives it end its run.
		err = errors.Join(err, l.Cancel(context.WithoutCancel(ctx), shadow.ID))

		return store.ReplayItem{}, fmt.Errorf("replay: run shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	// A shadow cancelled outside the replay has no outcome to compare, so the replay is no comparison base.
	if status == engine.StatusCancelled {
		return store.ReplayItem{}, fmt.Errorf("replay: shadow %d of task %d was cancelled outside the replay", shadow.ID, src.ID)
	}

	item, err := record(ctx, s, shadow)
	if err != nil {
		return store.ReplayItem{}, fmt.Errorf("replay: record shadow %d of task %d: %w", shadow.ID, src.ID, err)
	}

	// Nothing resumes a shadow, so once its stop is recorded a cancel ends its run and removes its volumes.
	if status == engine.StatusBlocked {
		if err = l.Cancel(ctx, shadow.ID); err == nil {
			_, err = drive(ctx, shadow.ID)
		}

		if err != nil {
			return store.ReplayItem{}, fmt.Errorf("replay: cancel shadow %d of task %d: %w", shadow.ID, src.ID, err)
		}
	}

	item.ReplayID, item.SourceTaskID = r.ID, src.ID

	return item, nil
}

// answerSuggested answers the shadow's open question with the answers the
// agent suggested, each after its question, when it suggested one for every question.
func answerSuggested(ctx context.Context, l *engine.Loop, taskID int64) (bool, error) {
	snap, err := l.Snapshot(ctx, taskID)
	if err != nil {
		return false, err
	}

	f := snap.Facts
	if f.OpenStop == nil || f.OpenStop.Reason != engine.StopQuestion || f.Session == nil {
		return false, nil
	}

	questions, answers := f.Session.Questions, f.Session.SuggestedAnswers
	if len(questions) == 0 || len(answers) != len(questions) || slices.Contains(answers, "") {
		return false, nil
	}

	pairs := make([]string, len(questions))
	for i, q := range questions {
		pairs[i] = q + "\n" + answers[i]
	}

	if err := l.Answer(ctx, taskID, strings.Join(pairs, "\n\n")); err != nil {
		return false, err
	}

	return true, nil
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

	item.Turns, item.Duration = engine.TurnsAndDuration(events)

	for _, e := range events {
		switch e.Type {
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

			if p.Outcome == engine.OutcomeBlocked {
				item.Asked += len(p.Questions)
			}
		case engine.Exported:
			var p engine.ExportedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return store.ReplayItem{}, fmt.Errorf("event %d: %w", e.ID, err)
			}

			item.DiffPath = p.DiffPath
		}
	}

	for _, u := range usage {
		item.Tokens += u.Input + u.Output + u.CacheRead + u.CacheWrite
		item.CostEstimateUSD += u.CostEstimateUSD
	}

	return item, nil
}
