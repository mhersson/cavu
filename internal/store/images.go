package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Image is one image of a task. Its contents are kept once per SHA-256, in a
// file of that name in the images directory beside the database.
type Image struct {
	ID, TaskID int64
	Name, Type string
	Size       int64
	SHA256     string
	CreatedAt  time.Time
}

const imageColumns = `id, task_id, name, type, size, sha256, created_at`

// ErrImagesFull is wrapped by PutImage's refusal of an image that would take
// its task's images past the most they may hold together.
var ErrImagesFull = errors.New("the task's images would hold more than they may together")

func scanImage(sc scanner) (Image, error) {
	var (
		img     Image
		created int64
	)

	if err := sc.Scan(&img.ID, &img.TaskID, &img.Name, &img.Type, &img.Size, &img.SHA256, &created); err != nil {
		return Image{}, err
	}

	img.CreatedAt = time.UnixMilli(created)

	return img, nil
}

// PutImage keeps what r holds as an image of img.TaskID with img's name and
// type, and returns it with its id, size, SHA-256 and time. Contents another
// image holds already are kept once. An image that would take the task's
// images past most bytes together is ErrImagesFull: r is read only until it
// passes the room left, and the room is checked again as the row is written,
// so of uploads that each fit alone but not together, only those that fit are kept.
func (s *Store) PutImage(ctx context.Context, img Image, r io.Reader, most int64) (Image, error) {
	held, err := s.imagesHeld(ctx, img.TaskID)
	if err != nil {
		return Image{}, fmt.Errorf("put image: %w", err)
	}

	room := most - held

	// One byte past the room is enough to know the image does not fit.
	tmp, sum, size, err := s.receive(io.LimitReader(r, max(min(room, math.MaxInt64-1)+1, 0)))
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = os.Remove(tmp) }() // gone already once placed

	if size > room {
		return Image{}, fmt.Errorf("put image: %w", ErrImagesFull)
	}

	img.Size, img.SHA256, img.CreatedAt = size, sum, time.UnixMilli(time.Now().UnixMilli())

	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()

	if err := s.place(tmp, sum); err != nil {
		return Image{}, err
	}

	// One statement, so no other upload's row lands between the sum and the insert.
	err = s.db.QueryRowContext(ctx, `INSERT INTO task_images (task_id, name, type, size, sha256, created_at)
		SELECT ?, ?, ?, ?, ?, ? WHERE (SELECT COALESCE(SUM(size), 0) FROM task_images WHERE task_id = ?) <= ?
		RETURNING id`,
		img.TaskID, img.Name, img.Type, img.Size, img.SHA256, img.CreatedAt.UnixMilli(), img.TaskID, most-size).Scan(&img.ID)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrImagesFull
	}

	if err != nil {
		s.unlink(ctx, sum)

		return Image{}, fmt.Errorf("put image: %w", err)
	}

	return img, nil
}

// imagesHeld is what the task's images hold together, in bytes.
func (s *Store) imagesHeld(ctx context.Context, taskID int64) (int64, error) {
	var held int64

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM task_images WHERE task_id = ?`, taskID).Scan(&held)

	return held, err
}

// receive writes r to a temporary file in the images directory, hashing it as
// it goes, and gives the file's path, its SHA-256 in hex and its size.
func (s *Store) receive(r io.Reader) (string, string, int64, error) {
	if err := os.MkdirAll(s.images, 0o700); err != nil {
		return "", "", 0, fmt.Errorf("put image: %w", err)
	}

	f, err := os.CreateTemp(s.images, "upload-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("put image: %w", err)
	}

	h := sha256.New()

	size, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync() // else a crash could leave a name with less behind it
	}

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(f.Name())

		return "", "", 0, fmt.Errorf("put image: %w", err)
	}

	return f.Name(), hex.EncodeToString(h.Sum(nil)), size, nil
}

// place renames the upload at tmp to sum, unless those contents are there.
func (s *Store) place(tmp, sum string) error {
	to := filepath.Join(s.images, sum)

	switch _, err := os.Lstat(to); {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("put image: %w", err)
	}

	if err := os.Rename(tmp, to); err != nil {
		return fmt.Errorf("put image: %w", err)
	}

	return nil
}

// TaskImages returns the task's images in the order added.
func (s *Store) TaskImages(ctx context.Context, taskID int64) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+imageColumns+` FROM task_images WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("task images: %w", err)
	}
	defer rows.Close()

	var out []Image

	for rows.Next() {
		img, err := scanImage(rows)
		if err != nil {
			return nil, fmt.Errorf("task images: %w", err)
		}

		out = append(out, img)
	}

	return out, rows.Err()
}

// OpenImage returns the task's image id with its contents open for reading,
// or ErrNotFound when the task has no such image.
func (s *Store) OpenImage(ctx context.Context, taskID, id int64) (Image, *os.File, error) {
	img, err := scanImage(s.db.QueryRowContext(ctx, `SELECT `+imageColumns+` FROM task_images WHERE task_id = ? AND id = ?`, taskID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, nil, fmt.Errorf("image %d of task %d: %w", id, taskID, ErrNotFound)
	}

	if err != nil {
		return Image{}, nil, fmt.Errorf("open image: %w", err)
	}

	f, err := os.Open(filepath.Join(s.images, img.SHA256))
	if err != nil {
		return Image{}, nil, fmt.Errorf("open image %d: %w", id, err)
	}

	return img, f, nil
}

// RemoveImages removes the task's images ids, or none and ErrNotFound when the
// task lacks one, then the contents no image names any longer.
func (s *Store) RemoveImages(ctx context.Context, taskID int64, ids []int64) error {
	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("remove images: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sums := make([]string, len(ids))

	for i, id := range ids {
		err := tx.QueryRowContext(ctx, `DELETE FROM task_images WHERE task_id = ? AND id = ? RETURNING sha256`, taskID, id).Scan(&sums[i])
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("image %d of task %d: %w", id, taskID, ErrNotFound)
		}

		if err != nil {
			return fmt.Errorf("remove images: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("remove images: %w", err)
	}

	s.unlink(ctx, sums...)

	return nil
}

// unlink removes the contents of each sum no image names, with imagesMu held.
// A failure leaves only contents nothing reads, so it is logged, not returned.
func (s *Store) unlink(ctx context.Context, sums ...string) {
	ctx = context.WithoutCancel(ctx)

	for _, sum := range sums {
		var named bool

		err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM task_images WHERE sha256 = ?)`, sum).Scan(&named)
		if err == nil && !named {
			err = os.Remove(filepath.Join(s.images, sum))
		}

		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.ErrorContext(ctx, "remove an image's contents", "sha256", sum, "err", err)
		}
	}
}

// CopyImages copies the images of task from to task to, in their order,
// sharing their contents. It needs no lock: it names only contents rows name.
func (s *Store) CopyImages(ctx context.Context, from, to int64) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO task_images (task_id, name, type, size, sha256, created_at)
		SELECT ?, name, type, size, sha256, created_at FROM task_images WHERE task_id = ? ORDER BY id`, to, from); err != nil {
		return fmt.Errorf("copy images: %w", err)
	}

	return nil
}
