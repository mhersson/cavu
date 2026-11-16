package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run is one execution of the agent on a task.
type Run struct {
	ID, TaskID         int64
	Reason             string // "initial" or "retry"
	SessionID, Outcome string
	StartedAt          time.Time
	EndedAt            *time.Time
}

const runColumns = `id, task_id, reason, session_id, outcome, started_at, ended_at`

func scanRun(sc scanner) (Run, error) {
	var (
		r       Run
		started int64
		ended   sql.NullInt64
	)

	if err := sc.Scan(&r.ID, &r.TaskID, &r.Reason, &r.SessionID, &r.Outcome, &started, &ended); err != nil {
		return Run{}, err
	}

	r.StartedAt = time.UnixMilli(started)

	if ended.Valid {
		t := time.UnixMilli(ended.Int64)
		r.EndedAt = &t
	}

	return r, nil
}

func endedArg(t *time.Time) any {
	if t == nil {
		return nil
	}

	return t.UnixMilli()
}

// CreateRun inserts r and returns it with its id; a zero StartedAt becomes now.
func (s *Store) CreateRun(ctx context.Context, r Run) (Run, error) {
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (task_id, reason, session_id, outcome,
		started_at, ended_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.TaskID, r.Reason, r.SessionID, r.Outcome, r.StartedAt.UnixMilli(), endedArg(r.EndedAt))
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}

	r.ID, err = res.LastInsertId()
	if err != nil {
		return Run{}, fmt.Errorf("create run: %w", err)
	}

	return r, nil
}

// UpdateRun rewrites every column but StartedAt of the run with r.ID, or returns ErrNotFound.
func (s *Store) UpdateRun(ctx context.Context, r Run) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET task_id = ?, reason = ?, session_id = ?,
		outcome = ?, ended_at = ? WHERE id = ?`,
		r.TaskID, r.Reason, r.SessionID, r.Outcome, endedArg(r.EndedAt), r.ID)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}

// OpenRun returns the task's newest run that has not ended; false when none.
func (s *Store) OpenRun(ctx context.Context, taskID int64) (Run, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs
		WHERE task_id = ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}

	if err != nil {
		return Run{}, false, fmt.Errorf("open run: %w", err)
	}

	return r, true, nil
}
