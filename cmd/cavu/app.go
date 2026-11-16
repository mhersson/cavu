package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/store"
)

// app is a verb's handle on the data directory.
type app struct {
	dir   string
	store *store.Store
	vault *auth.Vault
	held  *os.File // the lock file, while the lock is held
}

// openApp opens the data directory, creating it, the master key and the
// instance id on first use. It takes no lock and detects no runtime, so a verb
// that needs neither works on a machine without one and beside a running task.
func openApp(ctx context.Context, dir string) (*app, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}

	key, _, err := auth.LoadOrCreateMasterKey(filepath.Join(dir, "master.key"))
	if err != nil {
		return nil, err
	}

	s, err := store.Open(ctx, filepath.Join(dir, "cavu.db"))
	if err != nil {
		return nil, err
	}

	a := &app{dir: dir, store: s}

	if a.vault, err = auth.NewVault(s, key); err == nil {
		err = ensureInstanceID(ctx, s)
	}

	if err != nil {
		_ = s.Close()

		return nil, err
	}

	return a, nil
}

func ensureInstanceID(ctx context.Context, s *store.Store) error {
	if _, ok, err := s.Setting(ctx, "instance_id"); err != nil || ok {
		return err
	}

	id := make([]byte, 8)
	_, _ = rand.Read(id) // never returns an error

	return s.SetSetting(ctx, "instance_id", hex.EncodeToString(id))
}

// lock holds the data directory's lock until close or the process exits.
// The file is never removed: a new file would let a second holder in.
func (a *app) lock() error {
	path := filepath.Join(a.dir, "lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a descriptor fits in an int
		_ = f.Close()

		return fmt.Errorf("another cavu holds %s; wait for it to end: %w", path, err)
	}

	a.held = f

	return nil
}

func (a *app) close() {
	_ = a.store.Close()

	if a.held != nil {
		_ = a.held.Close()
	}
}

// verbFlags is the flag set of a verb that opens the app.
type verbFlags struct {
	*flag.FlagSet

	dataDir *string
}

func newVerbFlags(usage string, stderr io.Writer) verbFlags {
	fs := flag.NewFlagSet("cavu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s\n", usage)
		fs.PrintDefaults()
	}

	dir := fs.String("data-dir", "", "the data directory (default: $CAVU_DATA_DIR, else $XDG_DATA_HOME/cavu, else ~/.local/share/cavu)")

	return verbFlags{FlagSet: fs, dataDir: dir}
}

// parse returns the verb's n positional arguments. They come before the flags,
// because the flag package stops at the first positional argument.
func (f verbFlags) parse(args []string, n int) ([]string, error) {
	if len(args) < n {
		return nil, f.usageError()
	}

	if err := f.Parse(args[n:]); err != nil {
		return nil, exitError{code: 2, err: err}
	}

	if f.NArg() != 0 {
		return nil, f.usageError()
	}

	return args[:n], nil
}

func (f verbFlags) usageError() error {
	f.Usage()

	return exitError{code: 2}
}

func (f verbFlags) open(ctx context.Context) (*app, error) {
	dir, err := dataDir(*f.dataDir)
	if err != nil {
		return nil, err
	}

	return openApp(ctx, dir)
}

func dataDir(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}

	if dir := os.Getenv("CAVU_DATA_DIR"); dir != "" {
		return dir, nil
	}

	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "cavu"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("data directory: %w", err)
	}

	return filepath.Join(home, ".local", "share", "cavu"), nil
}
