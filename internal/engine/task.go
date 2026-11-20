package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/store"
)

// ErrRefused is wrapped by every refusal of a task's content or of a human
// action its state makes meaningless, and by nothing else.
var ErrRefused = errors.New("refused")

// ErrNoBaseline is wrapped, beside ErrRefused, by the refusal to start a task
// whose project has neither a passed baseline nor a waived verify.
var ErrNoBaseline = errors.New("neither a passed baseline nor a waived verify")

const maxTitle = 200

// AddTask stores a new draft task that holds only what a task may hold, and its
// dependencies, and with start starts it. A start the project cannot take is
// refused before anything is stored.
func (l *Loop) AddTask(ctx context.Context, t store.Task, dependsOn []int64, start bool) (store.Task, error) {
	if err := checkTitle(t.Title); err != nil {
		return store.Task{}, err
	}

	if start {
		p, err := l.Store.Project(ctx, t.ProjectID)
		if err != nil {
			return store.Task{}, fmt.Errorf("project %d: %w", t.ProjectID, err)
		}

		if !Runnable(p) {
			return store.Task{}, noBaseline(p)
		}
	}

	if err := checkDeps(ctx, l.Store, 0, dependsOn); err != nil {
		return store.Task{}, err
	}

	t, err := l.Store.CreateTask(ctx, t)
	if err != nil {
		return store.Task{}, err
	}

	if err := l.Store.SetTaskDeps(ctx, t.ID, dependsOn); err != nil {
		return store.Task{}, fmt.Errorf("task %d: %w", t.ID, err)
	}

	if start {
		if err := l.Start(ctx, t.ID); err != nil {
			return store.Task{}, err
		}
	}

	return t, nil
}

func noBaseline(p store.Project) error {
	return refuse("project %q has %w", p.Name, ErrNoBaseline)
}

// EditTask writes a task's title, body and dependencies until its first run
// starts, and never once it is cancelled.
func EditTask(ctx context.Context, s Store, t store.Task, dependsOn []int64) error {
	if err := checkTitle(t.Title); err != nil {
		return err
	}

	stored, err := s.Task(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("task %d: %w", t.ID, err)
	}

	events, err := s.EventsOmitting(ctx, t.ID, ProgressTypes...)
	if err != nil {
		return fmt.Errorf("task %d: %w", t.ID, err)
	}

	switch {
	case hadRun(events):
		return refuse("task %d has started a run", t.ID)
	case Reduce(events).Cancelled:
		return refuse("task %d is cancelled", t.ID)
	}

	if err := checkDeps(ctx, s, t.ID, dependsOn); err != nil {
		return err
	}

	stored.Title, stored.Body = t.Title, t.Body
	if err := s.UpdateTask(ctx, stored); err != nil {
		return fmt.Errorf("task %d: %w", t.ID, err)
	}

	if err := s.SetTaskDeps(ctx, t.ID, dependsOn); err != nil {
		return fmt.Errorf("task %d: %w", t.ID, err)
	}

	return nil
}

func checkTitle(title string) error {
	switch {
	case strings.TrimSpace(title) == "":
		return refuse("title is required")
	case utf8.RuneCountInString(title) > maxTitle:
		return refuse("title is over %d characters", maxTitle)
	case strings.ContainsFunc(title, unicode.IsControl):
		return refuse("title holds a line break or another control character")
	}

	return nil
}

// checkDeps refuses a dependency that does not exist, and one that is the task
// itself or leads back to it. A new task, id 0, has nothing that leads back to it.
func checkDeps(ctx context.Context, s Store, taskID int64, dependsOn []int64) error {
	for _, d := range dependsOn {
		if _, err := s.Task(ctx, d); errors.Is(err, store.ErrNotFound) {
			return refuse("task %d does not exist", d)
		} else if err != nil {
			return fmt.Errorf("dependency %d: %w", d, err)
		}
	}

	if taskID == 0 {
		return nil
	}

	seen, next := map[int64]bool{}, slices.Clone(dependsOn)

	for len(next) > 0 {
		d := next[len(next)-1]
		next = next[:len(next)-1]

		if d == taskID {
			return refuse("task %d would wait on itself", taskID)
		}

		if seen[d] {
			continue
		}

		seen[d] = true

		more, err := s.TaskDeps(ctx, d)
		if err != nil {
			return fmt.Errorf("dependency %d: %w", d, err)
		}

		next = append(next, more...)
	}

	return nil
}

// Start records that a person started a draft task. A started task is left
// as it is. A cancelled task, and one whose project has neither a passed
// baseline nor a waived verify, is refused.
func (l *Loop) Start(ctx context.Context, taskID int64) error {
	return l.human(ctx, taskID, TaskStarted, struct{}{}, func(f Facts, p store.Project) (bool, error) {
		switch {
		case f.Cancelled:
			return false, refuse("the task is cancelled")
		case f.Started:
			return false, nil
		case !Runnable(p):
			return false, noBaseline(p)
		}

		return true, nil
	})
}

// Answer records a person's answer to the question that stopped the task.
func (l *Loop) Answer(ctx context.Context, taskID int64, text string) error {
	text = strings.TrimSpace(text)

	return l.human(ctx, taskID, HumanAnswered, HumanAnsweredPayload{Text: text}, func(f Facts, _ store.Project) (bool, error) {
		switch {
		case f.OpenStop == nil || f.OpenStop.Reason != StopQuestion:
			return false, refuse("the task has no open question")
		case text == "":
			return false, refuse("the answer is blank")
		}

		return true, nil
	})
}

// Retry records a person's retry of the open stop on a task that is not cancelled.
func (l *Loop) Retry(ctx context.Context, taskID int64) error {
	return l.human(ctx, taskID, HumanRetried, struct{}{}, func(f Facts, _ store.Project) (bool, error) {
		switch {
		case f.Cancelled:
			return false, refuse("the task is cancelled")
		case f.OpenStop == nil:
			return false, refuse("the task has no open stop")
		}

		return true, nil
	})
}

// Cancel records that a person cancelled a task that is not yet cancelled or done.
func (l *Loop) Cancel(ctx context.Context, taskID int64) error {
	return l.human(ctx, taskID, HumanCancelled, struct{}{}, func(f Facts, _ store.Project) (bool, error) {
		if s := StatusOf(f); s == StatusCancelled || s == StatusDone {
			return false, refuse("the task is %s", s)
		}

		return true, nil
	})
}

// Done records that the change of a task in review, which has no pull
// request, was merged.
func (l *Loop) Done(ctx context.Context, taskID int64) error {
	return l.human(ctx, taskID, Merged, struct{}{}, func(f Facts, _ store.Project) (bool, error) {
		if f.PR != nil {
			return false, refuse("the task has a pull request, %s", f.PR.URL)
		}

		if s := StatusOf(f); s != StatusInReview {
			return false, refuse("the task is %s, not %s", s, StatusInReview)
		}

		return true, nil
	})
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// human records the user's event after guard allows it on the task's events (false: refuse with
// its error, or with none do nothing); a write that lost to a newer event reads and guards again, up to maxConflicts times.
func (l *Loop) human(ctx context.Context, taskID int64, typ string, payload any, guard func(Facts, store.Project) (bool, error)) error {
	var err error

	for range maxConflicts {
		if err = l.humanOnce(ctx, taskID, typ, payload, guard); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}

	return err
}

func (l *Loop) humanOnce(ctx context.Context, taskID int64, typ string, payload any, guard func(Facts, store.Project) (bool, error)) error {
	t, err := l.Store.Task(ctx, taskID)
	if err != nil {
		return fmt.Errorf("task %d: %w", taskID, err)
	}

	proj, err := l.Store.Project(ctx, t.ProjectID)
	if err != nil {
		return fmt.Errorf("task %d: project: %w", taskID, err)
	}

	events, err := l.Store.Events(ctx, taskID, 0, 0)
	if err != nil {
		return fmt.Errorf("task %d: %w", taskID, err)
	}

	var last int64
	if len(events) > 0 {
		last = events[len(events)-1].ID
	}

	if ok, err := guard(Reduce(events), proj); !ok {
		return err
	}

	p, err := l.newPass(ctx, taskID)
	if err != nil {
		return err
	}

	return p.write(ctx, last, 0, nil, draft{typ, actorUser, payload})
}
