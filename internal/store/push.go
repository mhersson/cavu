package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PushSubscription is a browser's Web Push subscription, its keys base64url as the browser gave them.
type PushSubscription struct {
	ID                             int64
	Endpoint, P256DH, Auth, Origin string
	CreatedAt                      time.Time
	LastOKAt                       *time.Time
}

// Notified is the way a task needed a person, and since when, as last notified.
type Notified struct {
	Way   string
	Since time.Time
}

// PutPushSubscription stores sub, or replaces the keys and origin of the
// subscription with its endpoint, and returns the subscription's id.
func (s *Store) PutPushSubscription(ctx context.Context, sub PushSubscription) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO push_subscriptions (endpoint, p256dh, auth, origin, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, origin = excluded.origin
		RETURNING id`, sub.Endpoint, sub.P256DH, sub.Auth, sub.Origin, sub.CreatedAt.UnixMilli()).Scan(&id); err != nil {
		return 0, fmt.Errorf("write push subscription: %w", err)
	}

	return id, nil
}

// PushSubscriptions returns every subscription, ordered by id.
func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, endpoint, p256dh, auth, origin, created_at, last_ok_at FROM push_subscriptions ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list push subscriptions: %w", err)
	}
	defer rows.Close()

	var out []PushSubscription

	for rows.Next() {
		var (
			p       PushSubscription
			created int64
			lastOK  sql.NullInt64
		)

		if err := rows.Scan(&p.ID, &p.Endpoint, &p.P256DH, &p.Auth, &p.Origin, &created, &lastOK); err != nil {
			return nil, fmt.Errorf("list push subscriptions: %w", err)
		}

		p.CreatedAt = time.UnixMilli(created)

		if lastOK.Valid {
			at := time.UnixMilli(lastOK.Int64)
			p.LastOKAt = &at
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

// RemovePushSubscription deletes the subscription id; an unknown id returns ErrNotFound.
func (s *Store) RemovePushSubscription(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("remove push subscription: %w", err)
	}

	return rowsAffectedOrNotFound(res)
}

// PushDelivered records that the push service accepted a push for id at at.
// An unknown id is a no-op: the subscription may be removed while a push is out.
func (s *Store) PushDelivered(ctx context.Context, id int64, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_ok_at = ? WHERE id = ?`, at.UnixMilli(), id); err != nil {
		return fmt.Errorf("record push delivered: %w", err)
	}

	return nil
}

// PushNotified returns what each task was last notified of, by task id.
func (s *Store) PushNotified(ctx context.Context) (map[int64]Notified, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT task_id, way, since FROM push_notified`)
	if err != nil {
		return nil, fmt.Errorf("list push notified: %w", err)
	}
	defer rows.Close()

	out := map[int64]Notified{}

	for rows.Next() {
		var (
			id    int64
			n     Notified
			since int64
		)

		if err := rows.Scan(&id, &n.Way, &since); err != nil {
			return nil, fmt.Errorf("list push notified: %w", err)
		}

		n.Since = time.UnixMilli(since)
		out[id] = n
	}

	return out, rows.Err()
}

// MarkPushNotified records n as what the task was last notified of.
func (s *Store) MarkPushNotified(ctx context.Context, taskID int64, n Notified) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO push_notified (task_id, way, since) VALUES (?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET way = excluded.way, since = excluded.since`, taskID, n.Way, n.Since.UnixMilli()); err != nil {
		return fmt.Errorf("mark push notified: %w", err)
	}

	return nil
}
