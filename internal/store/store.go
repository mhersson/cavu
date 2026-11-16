// Package store persists CAVU's state in one SQLite file: projects, tasks,
// runs, an append-only event log, usage, replays, settings and sealed secrets.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	sqliteConstraintUnique = 2067 // SQLITE_CONSTRAINT_UNIQUE
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

	if err := migrate(ctx, db, ms); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error

	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
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
