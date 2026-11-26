package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Task is a unit of work for the agent.
type Task struct {
	ID, ProjectID          int64
	Title, Body, Requester string
	BaseCommit             string // set at the first prepare, then fixed
	Reference              bool   // marked for replay
	ReplayID               int64  // non-zero for a replay's shadow task
	Overrides              map[string]string
	CreatedAt              time.Time
}

const taskColumns = `id, project_id, title, body, requester, base_commit, reference, replay_id, overrides, created_at`

func scanTask(sc scanner) (Task, error) {
	var (
		t         Task
		overrides string
		created   int64
	)

	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Body, &t.Requester, &t.BaseCommit,
		&t.Reference, &t.ReplayID, &overrides, &created); err != nil {
		return Task{}, err
	}

	if err := json.Unmarshal([]byte(overrides), &t.Overrides); err != nil {
		return Task{}, fmt.Errorf("decode task %d: %w", t.ID, err)
	}

	t.CreatedAt = time.UnixMilli(created)

	return t, nil
}

// CreateTask inserts t and returns it with its id; a zero CreatedAt becomes now.
// A removed or unknown project returns ErrNotFound.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, error) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}

	overrides, err := json.Marshal(t.Overrides)
	if err != nil {
		return Task{}, fmt.Errorf("encode task: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks (project_id, title, body, requester,
		base_commit, reference, replay_id, overrides, created_at)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ? FROM projects WHERE id = ? AND removed_at IS NULL`,
		t.Title, t.Body, t.Requester, t.BaseCommit, t.Reference, t.ReplayID,
		string(overrides), t.CreatedAt.UnixMilli(), t.ProjectID)
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	if err := rowsAffectedOrNotFound(res); err != nil {
		return Task{}, fmt.Errorf("create task: project %d: %w", t.ProjectID, err)
	}

	t.ID, err = res.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	return t, nil
}

// UpdateTask rewrites every column but CreatedAt of the task with t.ID, or returns ErrNotFound.
func (s *Store) UpdateTask(ctx context.Context, t Task) error {
	overrides, err := json.Marshal(t.Overrides)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET project_id = ?, title = ?, body = ?,
		requester = ?, base_commit = ?, reference = ?, replay_id = ?, overrides = ? WHERE id = ?`,
		t.ProjectID, t.Title, t.Body, t.Requester, t.BaseCommit, t.Reference, t.ReplayID,
		string(overrides), t.ID)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}

// Task returns the task with id, or ErrNotFound.
func (s *Store) Task(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}

	return t, err
}

// Tasks returns tasks oldest first; replay shadow tasks only with includeReplays.
func (s *Store) Tasks(ctx context.Context, includeReplays bool) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks`
	if !includeReplays {
		q += ` WHERE replay_id = 0`
	}

	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var out []Task

	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}

		out = append(out, t)
	}

	return out, rows.Err()
}

// TaskEdit is a change to a task: the title, body and dependencies to write,
// and the ids of its images to remove. A nil field keeps what is stored when
// the edit commits; a set one is written even when it is empty.
type TaskEdit struct {
	Title, Body  *string
	DependsOn    *[]int64
	RemoveImages []int64
}

// EditTask writes what e sets of the task's title, body and dependencies and
// removes its images e.RemoveImages in one transaction, then the contents no
// image names any longer. A task, or an image of it, that is not there is
// ErrNotFound. expectLast is as KeepImage takes it: a mismatch is ErrConflict.
// On any error nothing is written.
func (s *Store) EditTask(ctx context.Context, taskID int64, e TaskEdit, expectLast int64) error {
	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("edit task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := newestEvent(ctx, tx, taskID, expectLast); err != nil {
		return fmt.Errorf("edit task: %w", err)
	}

	// An edit records no event, so expectLast does not see another edit: a field
	// left out is kept by the write itself, as it stands when this one commits.
	// A nil pointer is bound as NULL.
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET title = COALESCE(?, title), body = COALESCE(?, body) WHERE id = ?`,
		e.Title, e.Body, taskID)
	if err != nil {
		return fmt.Errorf("edit task: %w", err)
	}

	if err := rowsAffectedOrNotFound(res); err != nil {
		return fmt.Errorf("edit task %d: %w", taskID, err)
	}

	if e.DependsOn != nil {
		if err := replaceDeps(ctx, tx, taskID, *e.DependsOn); err != nil {
			return fmt.Errorf("edit task: %w", err)
		}
	}

	sums, err := deleteImages(ctx, tx, taskID, e.RemoveImages)
	if err != nil {
		return fmt.Errorf("edit task: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("edit task: %w", err)
	}

	s.unlink(ctx, sums...)

	return nil
}

// SetTaskDeps replaces the task's dependencies in one transaction.
func (s *Store) SetTaskDeps(ctx context.Context, taskID int64, dependsOn []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set task deps: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := replaceDeps(ctx, tx, taskID, dependsOn); err != nil {
		return fmt.Errorf("set task deps: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("set task deps: %w", err)
	}

	return nil
}

func replaceDeps(ctx context.Context, tx *sql.Tx, taskID int64, dependsOn []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_deps WHERE task_id = ?`, taskID); err != nil {
		return err
	}

	for _, d := range dependsOn {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO task_deps (task_id, depends_on_id) VALUES (?, ?)`, taskID, d); err != nil {
			return err
		}
	}

	return nil
}

// TaskDeps returns the ids the task depends on, ascending.
func (s *Store) TaskDeps(ctx context.Context, taskID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT depends_on_id FROM task_deps WHERE task_id = ? ORDER BY depends_on_id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("task deps: %w", err)
	}
	defer rows.Close()

	var out []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("task deps: %w", err)
		}

		out = append(out, id)
	}

	return out, rows.Err()
}
