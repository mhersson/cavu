package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Replay is one run of the reference set under a named configuration. Config
// holds the task overrides every shadow task of the replay carries. EndedAt is
// nil until the replay has recorded every item. Image is the sandbox image the
// replay ran under.
type Replay struct {
	ID        int64
	Name      string
	Image     string
	Config    map[string]string
	StartedAt time.Time
	EndedAt   *time.Time
}

// ReplayItem is the outcome of one source task's shadow task within a replay.
type ReplayItem struct {
	ReplayID, SourceTaskID, TaskID int64
	Verify                         string // passed, failed, timeout, waived, or "" when the run did not reach verify
	Stop                           string // stop reason, "" if none
	Asked                          bool   // the session came back blocked with a question
	Turns                          int
	Tokens                         int64
	CostEstimateUSD                float64
	Duration                       time.Duration
	DiffPath                       string
}

// CreateReplay inserts r and returns it with its id; a zero StartedAt becomes now.
func (s *Store) CreateReplay(ctx context.Context, r Replay) (Replay, error) {
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}

	config, err := json.Marshal(r.Config)
	if err != nil {
		return Replay{}, fmt.Errorf("encode replay: %w", err)
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO replays (name, config, image, started_at, ended_at) VALUES (?, ?, ?, ?, ?)`,
		r.Name, string(config), r.Image, toMillis(r.StartedAt), endedArg(r.EndedAt))
	if err != nil {
		return Replay{}, fmt.Errorf("create replay: %w", err)
	}

	r.ID, err = res.LastInsertId()
	if err != nil {
		return Replay{}, fmt.Errorf("create replay: %w", err)
	}

	return r, nil
}

// EndReplay marks the replay with id as ended at at, or returns ErrNotFound.
func (s *Store) EndReplay(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE replays SET ended_at = ? WHERE id = ?`, toMillis(at), id)
	if err != nil {
		return fmt.Errorf("end replay: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}

// PutReplayItem inserts i, replacing the row for the same replay and source task.
func (s *Store) PutReplayItem(ctx context.Context, i ReplayItem) error {
	if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO replay_items (replay_id, source_task_id,
		task_id, verify, stop, asked, turns, tokens, cost_estimate_usd, duration_ms, diff_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ReplayID, i.SourceTaskID, i.TaskID, i.Verify, i.Stop, i.Asked, i.Turns, i.Tokens,
		i.CostEstimateUSD, i.Duration.Milliseconds(), i.DiffPath); err != nil {
		return fmt.Errorf("put replay item: %w", err)
	}

	return nil
}

// ReplayItems returns the replay's items ordered by source task id.
func (s *Store) ReplayItems(ctx context.Context, replayID int64) ([]ReplayItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT replay_id, source_task_id, task_id, verify, stop, asked,
		turns, tokens, cost_estimate_usd, duration_ms, diff_path
		FROM replay_items WHERE replay_id = ? ORDER BY source_task_id`, replayID)
	if err != nil {
		return nil, fmt.Errorf("list replay items: %w", err)
	}
	defer rows.Close()

	var out []ReplayItem

	for rows.Next() {
		var (
			i  ReplayItem
			ms int64
		)

		if err := rows.Scan(&i.ReplayID, &i.SourceTaskID, &i.TaskID, &i.Verify, &i.Stop, &i.Asked,
			&i.Turns, &i.Tokens, &i.CostEstimateUSD, &ms, &i.DiffPath); err != nil {
			return nil, fmt.Errorf("list replay items: %w", err)
		}

		i.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, i)
	}

	return out, rows.Err()
}

// PreviousReplay returns the newest ended replay with an id below before, so a
// cut-off replay is never a comparison base; false when none.
func (s *Store) PreviousReplay(ctx context.Context, before int64) (Replay, bool, error) {
	var (
		r       Replay
		config  string
		started int64
		ended   sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `SELECT id, name, config, image, started_at, ended_at FROM replays
		WHERE id < ? AND ended_at IS NOT NULL ORDER BY id DESC LIMIT 1`, before).Scan(&r.ID, &r.Name, &config, &r.Image, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return Replay{}, false, nil
	}

	if err != nil {
		return Replay{}, false, fmt.Errorf("previous replay: %w", err)
	}

	if err := json.Unmarshal([]byte(config), &r.Config); err != nil {
		return Replay{}, false, fmt.Errorf("decode replay %d: %w", r.ID, err)
	}

	r.StartedAt = fromMillis(started)

	if ended.Valid {
		t := fromMillis(ended.Int64)
		r.EndedAt = &t
	}

	return r, true, nil
}
