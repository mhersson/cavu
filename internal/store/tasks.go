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

	t.CreatedAt = fromMillis(created)

	return t, nil
}

// CreateTask inserts t and returns it with its id; a zero CreatedAt becomes now.
func (s *Store) CreateTask(ctx context.Context, t Task) (Task, error) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}

	overrides, err := json.Marshal(t.Overrides)
	if err != nil {
		return Task{}, fmt.Errorf("encode task: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO tasks (project_id, title, body, requester,
		base_commit, reference, replay_id, overrides, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ProjectID, t.Title, t.Body, t.Requester, t.BaseCommit, t.Reference, t.ReplayID,
		string(overrides), toMillis(t.CreatedAt))
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	t.ID, err = res.LastInsertId()
	if err != nil {
		return Task{}, fmt.Errorf("create task: %w", err)
	}

	return t, nil
}

// UpdateTask rewrites the task with t.ID, or returns ErrNotFound.
func (s *Store) UpdateTask(ctx context.Context, t Task) error {
	overrides, err := json.Marshal(t.Overrides)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET project_id = ?, title = ?, body = ?,
		requester = ?, base_commit = ?, reference = ?, replay_id = ?, overrides = ?,
		created_at = ? WHERE id = ?`,
		t.ProjectID, t.Title, t.Body, t.Requester, t.BaseCommit, t.Reference, t.ReplayID,
		string(overrides), toMillis(t.CreatedAt), t.ID)
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
