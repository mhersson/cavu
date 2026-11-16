// Package store persists CAVU's state in one SQLite file: projects, tasks,
// runs, an append-only event log, usage, settings and sealed secrets.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a write loses against existing state.
	ErrConflict = errors.New("conflict")
)

// SQLite result codes.
const (
	sqliteBusy             = 5    // SQLITE_BUSY
	sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// The first connection's retries while another opener switches a fresh database to WAL.
const (
	connectAttempts = 50
	connectBackoff  = 20 * time.Millisecond
)

// Store is a handle on the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// dsn builds a file: URI by concatenation because url.URL puts a relative
// path in the authority, which the driver rejects. _txlock=immediate makes
// every transaction take the write lock up front, so read-then-write
// sequences hold across processes.
func dsn(path string) string {
	return "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
}

// openDB opens the database at path, creating its parent directory.
// SQLite has one writer; extra connections let readers proceed under WAL.
func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	return db, nil
}

// Open opens the database at path and applies any pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	return open(ctx, path, migrations)
}

func open(ctx context.Context, path string, ms []Migration) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	if err := connect(ctx, db); err != nil {
		_ = db.Close()

		return nil, err
	}

	if err := migrate(ctx, db, ms); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &Store{db: db}, nil
}

// connect opens the first connection, which switches a fresh database to WAL.
// SQLite fails one of two concurrent switches with SQLITE_BUSY at once, without
// calling the busy handler, so the switch is retried here.
func connect(ctx context.Context, db *sql.DB) error {
	err := db.PingContext(ctx)

	for attempt := 1; attempt < connectAttempts && isBusy(err); attempt++ {
		t := time.NewTimer(connectBackoff)

		select {
		case <-ctx.Done():
			t.Stop()

			return fmt.Errorf("store: connect: %w", ctx.Err())
		case <-t.C:
		}

		err = db.PingContext(ctx)
	}

	if err != nil {
		return fmt.Errorf("store: connect: %w", err)
	}

	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func isBusy(err error) bool {
	var se *sqlite.Error

	return errors.As(err, &se) && se.Code()&0xff == sqliteBusy
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error

	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
}

func toMillis(t time.Time) int64 {
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	return time.UnixMilli(ms)
}

// nullableID maps the zero id to NULL.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}

	return id
}

// rowsAffectedOrNotFound turns an update that touched no row into ErrNotFound.
func rowsAffectedOrNotFound(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if n == 0 {
		return ErrNotFound
	}

	return nil
}
