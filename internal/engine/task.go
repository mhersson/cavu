package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/store"
)

// ErrRefused is wrapped by every refusal of a task's content or of a human
// action its state makes meaningless, and by nothing else.
var ErrRefused = errors.New("refused")

const maxTitle = 200

// AddTask stores a new draft task that holds only what a task may hold.
func AddTask(ctx context.Context, s Store, t store.Task) (store.Task, error) {
	switch {
	case strings.TrimSpace(t.Title) == "":
		return store.Task{}, fmt.Errorf("%w: title is required", ErrRefused)
	case utf8.RuneCountInString(t.Title) > maxTitle:
		return store.Task{}, fmt.Errorf("%w: title is over %d characters", ErrRefused, maxTitle)
	case strings.ContainsFunc(t.Title, unicode.IsControl):
		return store.Task{}, fmt.Errorf("%w: title holds a line break or another control character", ErrRefused)
	}

	return s.CreateTask(ctx, t)
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
		case !p.VerifyWaived && (p.Baseline == nil || !p.Baseline.Passed):
			return false, refuse("project %q has neither a passed baseline nor a waived verify; check the project, or waive verify", p.Name)
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

// Cancel records that a person cancelled a task that is not yet cancelled or in review.
func (l *Loop) Cancel(ctx context.Context, taskID int64) error {
	return l.human(ctx, taskID, HumanCancelled, struct{}{}, func(f Facts, _ store.Project) (bool, error) {
		if s := StatusOf(f); s == StatusCancelled || s == StatusInReview {
			return false, refuse("the task is %s", s)
		}

		return true, nil
	})
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// human reads the task's events, lets guard decide (false: refuse with its
// error, or with none do nothing), and records the user's event through the
// loop's one write, only if no event came since the read.
func (l *Loop) human(ctx context.Context, taskID int64, typ string, payload any, guard func(Facts, store.Project) (bool, error)) error {
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
