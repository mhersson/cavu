package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Batch is tasks of one project lined up to run one after another into one
// pull request. Its state is not stored: it follows from its tasks' events and
// from the moments below, each nil until it happens.
type Batch struct {
	ID, ProjectID int64
	Name          string
	Version       int64   // raised by every WriteBatch
	BaseCommit    string  // empty until the first task's first prepare resolves it
	Tasks         []int64 // in order
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time // Finish here, which does not end the batch
	CancelledAt   *time.Time
	EndedAt       *time.Time
}

// BatchFilter picks batches: of one project unless ProjectID is 0, and ended
// or not unless Ended is nil.
type BatchFilter struct {
	ProjectID int64
	Ended     *bool
}

// BatchPlace is a task's place in its batch; BatchID 0 is none.
type BatchPlace struct {
	BatchID        int64
	Name           string
	Position, Size int // Position counts from 1
}

// TaskAppend is a check of one task's newest event and what is appended to it,
// as Append takes them: ExpectLast -1 skips the check, and no events leaves
// only the check.
type TaskAppend struct {
	TaskID, ExpectLast int64
	Events             []Event
	Usage              []Usage
}

// BatchWrite is one change to a batch, written all or nothing. A nil field
// keeps what is stored; Tasks, when set, is the whole list in order. Delete
// removes the batch and its list, and writes none of the fields.
type BatchWrite struct {
	Version                                     int64 // the version the caller read; -1 skips the check
	Appends                                     []TaskAppend
	Name, BaseCommit                            *string
	StartedAt, FinishedAt, CancelledAt, EndedAt *time.Time
	Tasks                                       *[]int64
	Delete                                      bool
}

const batchSelect = `SELECT id, project_id, name, version, base_commit, created_at,
	started_at, finished_at, cancelled_at, ended_at,
	(SELECT json_group_array(task_id ORDER BY position) FROM batch_tasks WHERE batch_id = batches.id)
	FROM batches `

func scanBatch(sc scanner) (Batch, error) {
	var (
		b                                 Batch
		created                           int64
		started, finished, cancelled, end sql.NullInt64
		tasks                             string
	)

	if err := sc.Scan(&b.ID, &b.ProjectID, &b.Name, &b.Version, &b.BaseCommit, &created,
		&started, &finished, &cancelled, &end, &tasks); err != nil {
		return Batch{}, err
	}

	if err := json.Unmarshal([]byte(tasks), &b.Tasks); err != nil {
		return Batch{}, fmt.Errorf("decode batch %d: %w", b.ID, err)
	}

	b.CreatedAt = time.UnixMilli(created)
	b.StartedAt, b.FinishedAt, b.CancelledAt, b.EndedAt = moment(started), moment(finished), moment(cancelled), moment(end)

	return b, nil
}

func moment(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}

	return new(time.UnixMilli(ms.Int64))
}

// millis maps a nil moment to NULL.
func millis(t *time.Time) any {
	if t == nil {
		return nil
	}

	return t.UnixMilli()
}

// CreateBatch inserts b with its tasks in order and makes appends, in one
// transaction, and returns b with its id, its first version and, when it had
// none, its time. A removed or unknown project is ErrNotFound; a task already
// in a batch, or one whose newest event is not the one appends expects, is
// ErrConflict. On any error nothing is written.
func (s *Store) CreateBatch(ctx context.Context, b Batch, appends []TaskAppend) (Batch, error) {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.UnixMilli(time.Now().UnixMilli())
	}

	b.Version = 1

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkTasks(ctx, tx, appends); err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}

	err = tx.QueryRowContext(ctx, `INSERT INTO batches (project_id, name, version, base_commit, created_at,
		started_at, finished_at, cancelled_at, ended_at)
		SELECT id, ?, ?, ?, ?, ?, ?, ?, ? FROM projects WHERE id = ? AND removed_at IS NULL RETURNING id`,
		b.Name, b.Version, b.BaseCommit, b.CreatedAt.UnixMilli(),
		millis(b.StartedAt), millis(b.FinishedAt), millis(b.CancelledAt), millis(b.EndedAt), b.ProjectID).Scan(&b.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, fmt.Errorf("create batch: project %d: %w", b.ProjectID, ErrNotFound)
	}

	if err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}

	if err := putBatchTasks(ctx, tx, b.ID, b.Tasks); err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}

	if err := appendTasks(ctx, tx, appends); err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Batch{}, fmt.Errorf("create batch: %w", err)
	}

	return b, nil
}

// WriteBatch makes w on the batch id in one transaction and gives its new
// version, 0 when w deletes it. A version other than w.Version, or a task
// whose newest event is not the one w.Appends expects, is ErrConflict, and so
// is a list that puts a task in two places or in a second batch. A batch that
// is not there is ErrNotFound. On any error nothing is written.
func (s *Store) WriteBatch(ctx context.Context, id int64, w BatchWrite) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int64

	err = tx.QueryRowContext(ctx, `SELECT version FROM batches WHERE id = ?`, id).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("write batch %d: %w", id, ErrNotFound)
	}

	if err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}

	if w.Version >= 0 && w.Version != version {
		return 0, fmt.Errorf("batch %d is at version %d, caller saw %d: %w", id, version, w.Version, ErrConflict)
	}

	if err := checkTasks(ctx, tx, w.Appends); err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}

	if w.Delete {
		version, err = 0, deleteBatch(ctx, tx, id)
	} else {
		version, err = updateBatch(ctx, tx, id, w)
	}

	if err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}

	if err := appendTasks(ctx, tx, w.Appends); err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("write batch: %w", err)
	}

	return version, nil
}

// updateBatch writes what w sets and gives the raised version.
func updateBatch(ctx context.Context, tx *sql.Tx, id int64, w BatchWrite) (int64, error) {
	var version int64

	// A nil pointer is bound as NULL, which keeps the column.
	if err := tx.QueryRowContext(ctx, `UPDATE batches SET version = version + 1,
		name = COALESCE(?, name), base_commit = COALESCE(?, base_commit),
		started_at = COALESCE(?, started_at), finished_at = COALESCE(?, finished_at),
		cancelled_at = COALESCE(?, cancelled_at), ended_at = COALESCE(?, ended_at)
		WHERE id = ? RETURNING version`,
		w.Name, w.BaseCommit, millis(w.StartedAt), millis(w.FinishedAt), millis(w.CancelledAt), millis(w.EndedAt), id,
	).Scan(&version); err != nil {
		return 0, err
	}

	if w.Tasks == nil {
		return version, nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_tasks WHERE batch_id = ?`, id); err != nil {
		return 0, err
	}

	return version, putBatchTasks(ctx, tx, id, *w.Tasks)
}

func deleteBatch(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_tasks WHERE batch_id = ?`, id); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, `DELETE FROM batches WHERE id = ?`, id)

	return err
}

// putBatchTasks writes the batch's tasks at positions from 1, in order.
func putBatchTasks(ctx context.Context, tx *sql.Tx, id int64, tasks []int64) error {
	for i, task := range tasks {
		_, err := tx.ExecContext(ctx, `INSERT INTO batch_tasks (batch_id, task_id, position) VALUES (?, ?, ?)`, id, task, i+1)
		if isUniqueViolation(err) {
			return fmt.Errorf("task %d is in a batch: %w", task, ErrConflict)
		}

		if err != nil {
			return fmt.Errorf("batch tasks: %w", err)
		}
	}

	return nil
}

func checkTasks(ctx context.Context, tx *sql.Tx, appends []TaskAppend) error {
	for _, a := range appends {
		if _, err := newestEvent(ctx, tx, a.TaskID, a.ExpectLast); err != nil {
			return err
		}
	}

	return nil
}

func appendTasks(ctx context.Context, tx *sql.Tx, appends []TaskAppend) error {
	for _, a := range appends {
		if _, err := insertEvents(ctx, tx, a.TaskID, a.Events, a.Usage); err != nil {
			return fmt.Errorf("task %d: %w", a.TaskID, err)
		}
	}

	return nil
}

// Batch returns the batch with id, or ErrNotFound.
func (s *Store) Batch(ctx context.Context, id int64) (Batch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, batchSelect+`WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Batch{}, fmt.Errorf("batch %d: %w", id, ErrNotFound)
	}

	if err != nil {
		return Batch{}, fmt.Errorf("batch %d: %w", id, err)
	}

	return b, nil
}

// Batches returns the batches f picks, oldest first.
func (s *Store) Batches(ctx context.Context, f BatchFilter) ([]Batch, error) {
	q, args := batchSelect+`WHERE TRUE`, []any{}
	if f.ProjectID != 0 {
		q, args = q+` AND project_id = ?`, append(args, f.ProjectID)
	}

	if f.Ended != nil {
		q, args = q+` AND (ended_at IS NOT NULL) = ?`, append(args, *f.Ended)
	}

	rows, err := s.db.QueryContext(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list batches: %w", err)
	}
	defer rows.Close()

	var out []Batch

	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("list batches: %w", err)
		}

		out = append(out, b)
	}

	return out, rows.Err()
}

// TaskBatch returns the task's place in its batch, the zero place when it is in none.
func (s *Store) TaskBatch(ctx context.Context, taskID int64) (BatchPlace, error) {
	var p BatchPlace

	err := s.db.QueryRowContext(ctx, `SELECT b.id, b.name, t.position,
		(SELECT COUNT(*) FROM batch_tasks c WHERE c.batch_id = b.id)
		FROM batch_tasks t JOIN batches b ON b.id = t.batch_id WHERE t.task_id = ?`, taskID).
		Scan(&p.BatchID, &p.Name, &p.Position, &p.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchPlace{}, nil
	}

	if err != nil {
		return BatchPlace{}, fmt.Errorf("task %d batch: %w", taskID, err)
	}

	return p, nil
}
