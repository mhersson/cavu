package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Event is one immutable fact in a task's history.
type Event struct {
	ID, TaskID, RunID int64 // RunID 0 = none
	At                time.Time
	Type, Actor       string // actor: user, engine, runner, agent
	Payload           json.RawMessage
}

// Usage is token and cost accounting for one source and model within a run.
type Usage struct {
	RunID                                int64
	Source                               string // "proxy" or "agent"
	Model                                string
	Input, Output, CacheRead, CacheWrite int64
	CostEstimateUSD                      float64
}

// Append adds events and usage rows in one transaction. expectLast is the id of
// the task's newest event the caller saw; -1 skips the check. A mismatch
// returns ErrConflict and writes nothing. Returns the id of the last event
// written, or the task's newest event id when none were.
func (s *Store) Append(ctx context.Context, taskID, expectLast int64, events []Event, usage []Usage) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	last, err := newestEvent(ctx, tx, taskID, expectLast)
	if err != nil {
		return 0, fmt.Errorf("append: %w", err)
	}

	written, err := insertEvents(ctx, tx, taskID, events, usage)
	if err != nil {
		return 0, fmt.Errorf("append: %w", err)
	}

	if written != 0 {
		last = written
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("append: commit: %w", err)
	}

	return last, nil
}

// insertEvents inserts the task's events, then its usage rows on the last of
// them, or on no event when there are none, and gives that event's id, 0 for none.
func insertEvents(ctx context.Context, tx *sql.Tx, taskID int64, events []Event, usage []Usage) (int64, error) {
	var last int64

	for _, e := range events {
		payload := e.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("null")
		}

		at := e.At
		if at.IsZero() {
			at = time.Now()
		}

		res, err := tx.ExecContext(ctx, `INSERT INTO events (task_id, run_id, at, type, actor, payload)
			VALUES (?, ?, ?, ?, ?, ?)`,
			taskID, nullableID(e.RunID), at.UnixMilli(), e.Type, e.Actor, string(payload))
		if err != nil {
			return 0, fmt.Errorf("insert event: %w", err)
		}

		if last, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	}

	for _, u := range usage {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage (task_id, run_id, event_id, source, model,
			input, output, cache_read, cache_write, cost_estimate_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			taskID, nullableID(u.RunID), nullableID(last), u.Source, u.Model,
			u.Input, u.Output, u.CacheRead, u.CacheWrite, u.CostEstimateUSD); err != nil {
			return 0, fmt.Errorf("insert usage: %w", err)
		}
	}

	return last, nil
}

// newestEvent is the id of the task's newest event, 0 when it has none, or
// ErrConflict when expectLast is not -1 and not that id.
func newestEvent(ctx context.Context, tx *sql.Tx, taskID, expectLast int64) (int64, error) {
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE task_id = ?`, taskID).Scan(&last); err != nil {
		return 0, fmt.Errorf("newest event: %w", err)
	}

	if expectLast >= 0 && expectLast != last {
		return 0, fmt.Errorf("task %d newest event is %d, caller saw %d: %w", taskID, last, expectLast, ErrConflict)
	}

	return last, nil
}

const eventsSelect = `SELECT id, task_id, run_id, at, type, actor, payload FROM events `

// Events returns the task's events with id greater than afterID, oldest first.
// limit 0 returns all.
func (s *Store) Events(ctx context.Context, taskID, afterID int64, limit int) ([]Event, error) {
	return s.EventsAfter(ctx, afterID, taskID, limit)
}

// EventsAfter returns events with an id above afterID, of every task or of
// taskID when it is not 0, in id order, at most limit (0 or less: no limit).
func (s *Store) EventsAfter(ctx context.Context, afterID, taskID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = -1
	}

	if taskID == 0 {
		return s.queryEvents(ctx, eventsSelect+`WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	}

	return s.queryEvents(ctx, eventsSelect+`WHERE task_id = ? AND id > ? ORDER BY id LIMIT ?`, taskID, afterID, limit)
}

// EventPage is a page of a task's events: those after the event After, oldest
// first, or with Newest those below it (all when it is 0), newest first; at
// most Limit (0 or less: no limit); and none whose actor is OmitActor, unless
// that is "".
type EventPage struct {
	After     int64
	Limit     int
	Newest    bool
	OmitActor string
}

// EventsPage returns a page of the task's events.
func (s *Store) EventsPage(ctx context.Context, taskID int64, p EventPage) ([]Event, error) {
	if p.Limit <= 0 {
		p.Limit = -1
	}

	where, order := `id > ?`, `id`
	if p.Newest {
		where, order = `id < ?`, `id DESC`

		if p.After == 0 {
			p.After = math.MaxInt64
		}
	}

	q, args := eventsSelect+`WHERE task_id = ? AND `+where, []any{taskID, p.After}
	if p.OmitActor != "" {
		q, args = q+` AND actor != ?`, append(args, p.OmitActor)
	}

	return s.queryEvents(ctx, q+` ORDER BY `+order+` LIMIT ?`, append(args, p.Limit)...)
}

// EventsOmitting returns the task's events but those of the given types, oldest first.
func (s *Store) EventsOmitting(ctx context.Context, taskID int64, omit ...string) ([]Event, error) {
	if omit == nil {
		omit = []string{}
	}

	b, err := json.Marshal(omit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	return s.queryEvents(ctx, eventsSelect+`WHERE task_id = ? AND type NOT IN (SELECT value FROM json_each(?)) ORDER BY id`, taskID, string(b))
}

// LastEventID returns the id of the task's newest event, 0 when it has none.
func (s *Store) LastEventID(ctx context.Context, taskID int64) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE task_id = ?`, taskID).Scan(&id); err != nil {
		return 0, fmt.Errorf("newest event: %w", err)
	}

	return id, nil
}

// LastEventIDs maps every task that has an event to its newest event id. It
// seeks each task's newest event in the index, so a scheduler's pass does not
// read the whole event history.
func (s *Store) LastEventIDs(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, last FROM (SELECT id,
		(SELECT MAX(e.id) FROM events e WHERE e.task_id = t.id) AS last FROM tasks t) WHERE last IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("newest events: %w", err)
	}
	defer rows.Close()

	out := map[int64]int64{}

	for rows.Next() {
		var task, id int64
		if err := rows.Scan(&task, &id); err != nil {
			return nil, fmt.Errorf("newest events: %w", err)
		}

		out[task] = id
	}

	return out, rows.Err()
}

func (s *Store) queryEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []Event

	for rows.Next() {
		var (
			e       Event
			runID   *int64
			at      int64
			payload string
		)

		if err := rows.Scan(&e.ID, &e.TaskID, &runID, &at, &e.Type, &e.Actor, &payload); err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}

		if runID != nil {
			e.RunID = *runID
		}

		e.At = time.UnixMilli(at)
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}

	return out, rows.Err()
}

// usageSelect and usageGroup sum usage rows per source and model around a
// query's own clauses; RunID is zero.
const (
	usageSelect = `SELECT source, model, SUM(input), SUM(output), SUM(cache_read), SUM(cache_write), SUM(cost_estimate_usd)
		FROM usage `
	usageGroup = ` GROUP BY source, model ORDER BY source, model`

	usageBetweenQuery = usageSelect + `JOIN events ON events.id = usage.event_id WHERE events.at >= ? AND events.at < ?` + usageGroup
)

// UsageTotals returns the task's usage summed per source and model. RunID is zero.
func (s *Store) UsageTotals(ctx context.Context, taskID int64) ([]Usage, error) {
	return s.sumUsage(ctx, usageSelect+`WHERE task_id = ?`+usageGroup, taskID)
}

// UsageSince sums the task's usage per source and model over events after afterEventID.
func (s *Store) UsageSince(ctx context.Context, taskID, afterEventID int64) ([]Usage, error) {
	return s.sumUsage(ctx, usageSelect+`WHERE task_id = ? AND event_id > ?`+usageGroup, taskID, afterEventID)
}

// UsageBetween sums every task's usage per source and model over events from
// from up to, not including, to.
func (s *Store) UsageBetween(ctx context.Context, from, to time.Time) ([]Usage, error) {
	return s.sumUsage(ctx, usageBetweenQuery, from.UnixMilli(), to.UnixMilli())
}

func (s *Store) sumUsage(ctx context.Context, query string, args ...any) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	defer rows.Close()

	var out []Usage

	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.Source, &u.Model, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostEstimateUSD); err != nil {
			return nil, fmt.Errorf("usage: %w", err)
		}

		out = append(out, u)
	}

	return out, rows.Err()
}
