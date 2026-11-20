package main

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/doctor"
	"github.com/mhersson/cavu/internal/driver/claudecode"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/push"
	"github.com/mhersson/cavu/internal/runner"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/store"
)

// app is a verb's handle on the data directory.
type app struct {
	dir        string
	store      *store.Store
	vault      *auth.Vault
	instanceID string
	held       *os.File // the lock file, while the lock is held
	image      string   // the image this process drives on, a person's choice and unchecked; "" for this binary's
	pushKey    *ecdsa.PrivateKey
}

// openApp opens the data directory, creating it, the master key, the instance
// id and the VAPID key on first use. It takes no lock and detects no runtime, so
// a verb that needs neither works on a machine without one and beside a running task.
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
		a.instanceID, err = ensureInstanceID(ctx, s)
	}

	if err == nil {
		a.pushKey, err = ensurePushKey(ctx, a.vault)
	}

	if err != nil {
		_ = s.Close()

		return nil, err
	}

	return a, nil
}

func ensureInstanceID(ctx context.Context, s *store.Store) (string, error) {
	if id, ok, err := s.Setting(ctx, "instance_id"); err != nil || ok {
		return id, err
	}

	b := make([]byte, 8)
	_, _ = rand.Read(b) // never returns an error
	id := hex.EncodeToString(b)

	return id, s.SetSetting(ctx, "instance_id", id)
}

// ensurePushKey is the instance's VAPID key, made once: every browser's
// subscription is bound to it.
func ensurePushKey(ctx context.Context, v *auth.Vault) (*ecdsa.PrivateKey, error) {
	sealed, ok, err := v.Get(ctx, "push.vapid_key")
	if err != nil {
		return nil, err
	}

	if ok {
		return push.DecodeKey(sealed)
	}

	key, err := push.NewKey()
	if err != nil {
		return nil, fmt.Errorf("vapid key: %w", err)
	}

	return key, v.Set(ctx, "push.vapid_key", push.EncodeKey(key))
}

// The roles a lock holder writes into the lock file.
const (
	roleServe = "serve"
	roleVerb  = "verb"
)

// lock takes the data directory's lock and writes role into its file. It holds
// the lock until close or the process exits. The file is never removed: a new
// file would let a second holder in.
func (a *app) lock(role string) error {
	path := filepath.Join(a.dir, "lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a descriptor fits in an int
		_ = f.Close()

		return fmt.Errorf("another cavu holds %s; wait for it to end: %w", path, err)
	}

	if err = f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(role+"\n"), 0)
	}

	if err != nil {
		_ = f.Close()

		return fmt.Errorf("lock: %w", err)
	}

	a.held = f

	return nil
}

// lockHolder is the role in the lock file when another process holds the lock.
func (a *app) lockHolder() (string, error) {
	b, err := os.ReadFile(filepath.Join(a.dir, "lock"))

	return strings.TrimSpace(string(b)), err
}

// serverHolds reports whether err is the lock refused because a server holds it.
func (a *app) serverHolds(err error) bool {
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}

	role, _ := a.lockHolder()

	return role == roleServe
}

// containerRuntime is what the verbs use of the container runtime.
type containerRuntime interface {
	BuildImage(ctx context.Context, o sandbox.BuildOptions, out io.Writer) error
	ImageInfo(ctx context.Context, name string) (sandbox.ImageLabels, bool, error)
	RemoveOrphans(ctx context.Context, instanceID string, keepRuns []int64) error
	Runner() engine.Runner
}

// hostRuntime is the runtime on this host, with a runner of Claude Code over
// it that reaches the forge through one client.
type hostRuntime struct {
	sandbox.Runtime

	forge *forge.Client
}

func (r hostRuntime) Runner() engine.Runner { return runner.New(r.Runtime, claudecode.New(), r.forge) }

func (r hostRuntime) RemoveOrphans(ctx context.Context, instanceID string, keepRuns []int64) error {
	return sandbox.RemoveOrphans(ctx, r.Runtime, instanceID, keepRuns)
}

// detectRuntime is the one seam to the container runtime: the unit tests
// replace it, so they start no container and run no runtime binary.
var detectRuntime = func(ctx context.Context, name string, f *forge.Client) (containerRuntime, error) {
	rt, err := sandbox.Detect(ctx, name)
	if err != nil {
		return nil, err
	}

	return hostRuntime{rt, f}, nil
}

// runtime detects the runtime the runtime setting names, else the first
// found, with the forge client its runners share, which trusts the CA bundle.
func (a *app) runtime(ctx context.Context) (containerRuntime, error) {
	name, _, err := a.store.Setting(ctx, "runtime")
	if err != nil {
		return nil, err
	}

	f, _, err := a.forgeClient(ctx)
	if err != nil {
		return nil, err
	}

	return detectRuntime(ctx, name, f)
}

// forgeClient is the forge client that trusts the CA bundle ca_bundle_path
// names, and the bundle, nil when unset.
func (a *app) forgeClient(ctx context.Context) (*forge.Client, []byte, error) {
	bundle, err := a.caBundle(ctx)
	if err != nil {
		return nil, nil, err
	}

	f, err := forge.New(forge.Options{CABundle: bundle, Version: version})
	if err != nil {
		return nil, nil, fmt.Errorf("ca_bundle_path: %w", err)
	}

	return f, bundle, nil
}

// caBundle reads the CA bundle the ca_bundle_path setting names, nil when unset.
func (a *app) caBundle(ctx context.Context) ([]byte, error) {
	path, _, err := a.store.Setting(ctx, "ca_bundle_path")
	if err != nil || path == "" {
		return nil, err
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ca_bundle_path: %w", err)
	}

	return b, nil
}

// drive takes what a verb needs before it starts a container: the lock, the
// runtime, and this binary's image with the configured CA bundle, unless the
// person named another image.
func (a *app) drive(ctx context.Context) (containerRuntime, error) {
	if err := a.lock(roleVerb); err != nil {
		return nil, err
	}

	rt, err := a.runtime(ctx)
	if err != nil {
		return nil, err
	}

	if a.image == "" {
		if err := a.checkImage(ctx, rt); err != nil {
			return nil, err
		}
	}

	return rt, nil
}

// pollEvery is how often a verb reads the store, and how often its own
// scheduler passes, so a task the verb starts beside it waits at most that long.
var pollEvery = 500 * time.Millisecond

// driven runs wait while something drives the tasks only admits: this
// process's own scheduler, under the lock, when the lock is free, else the
// server that holds it. Another verb holding the lock is an error naming it.
// Driving itself, it takes drive's lock, runtime and image check, then removes the orphans.
func (a *app) driven(ctx context.Context, only func(store.Task) bool, wait func(context.Context) error) error {
	rt, err := a.drive(ctx)
	if a.serverHolds(err) {
		return wait(ctx)
	}

	if err != nil {
		return err
	}

	if err := a.removeOrphans(ctx, rt); err != nil {
		return err
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// OnError runs on Run's goroutine, and failed is read once Run has returned.
	var failed error

	s := &engine.Scheduler{Loop: a.loop(rt), Tick: pollEvery, Only: only, OnError: func(_ int64, err error) {
		failed = errors.Join(failed, err)

		stop()
	}}

	ran := make(chan struct{})

	go func() {
		defer close(ran)

		_ = s.Run(ctx)
	}()

	err = wait(ctx)

	// A task rests while its drive still releases its run: cut there, the run
	// would keep its containers, and an exported run its volumes.
	for err == nil && ctx.Err() == nil && s.Driving() {
		select {
		case <-ctx.Done():
		case <-time.After(pollEvery):
		}
	}

	stop()
	<-ran

	if failed != nil {
		return failed
	}

	return err
}

// removeOrphans removes the instance's containers and the volumes of every run
// that is not open. With the lock held, no container of the instance is in use.
// A task cancelled at rest still holds its run, so the loop first ends that run.
func (a *app) removeOrphans(ctx context.Context, rt containerRuntime) error {
	tasks, err := a.store.Tasks(ctx, true)
	if err != nil {
		return err
	}

	for _, t := range tasks {
		snap, err := a.loop(rt).Snapshot(ctx, t.ID)
		if err != nil {
			return err
		}

		if engine.Decide(snap).Kind == engine.KindStop {
			if _, err := a.loop(rt).RunTask(ctx, t.ID); err != nil {
				return err
			}
		}
	}

	keep, err := a.store.OpenRunIDs(ctx)
	if err != nil {
		return err
	}

	return rt.RemoveOrphans(ctx, a.instanceID, keep)
}

// checkImage stops a verb before any container starts unless this binary's
// image is built, holds the Claude Code it pins and carries the configured CA
// bundle.
func (a *app) checkImage(ctx context.Context, rt containerRuntime) error {
	bundle, err := a.caBundle(ctx)
	if err != nil {
		return err
	}

	labels, found, err := rt.ImageInfo(ctx, sandbox.ImageName(version))
	if err != nil {
		return err
	}

	if c := doctor.CheckImage(labels, found, version, claudecode.Version, bundle); !c.OK {
		return fmt.Errorf("%s; fix: %s", c.Detail, c.Fix)
	}

	return nil
}

// loop is the engine over rt's runner, on the image actions names.
func (a *app) loop(rt containerRuntime) *engine.Loop {
	l := a.actions()
	l.Runner = rt.Runner()

	return l
}

// actions is the engine without a runner: enough to record what a person does.
func (a *app) actions() *engine.Loop {
	return &engine.Loop{Store: a.store, Vault: a.vault, Now: time.Now, DataDir: a.dir, Image: cmp.Or(a.image, sandbox.ImageName(version))}
}

func (a *app) close() {
	_ = a.store.Close()

	if a.held != nil {
		_ = a.held.Close()
	}
}

// verbFlags is a verb's flag set; dataDir is nil for a set made without --data-dir.
type verbFlags struct {
	*flag.FlagSet

	dataDir *string
}

// newVerbFlags is a verb's flag set with --data-dir.
func newVerbFlags(usage string, stderr io.Writer) verbFlags {
	f := newFlags(usage, stderr)
	f.dataDir = f.String("data-dir", "", "the data directory (default: $CAVU_DATA_DIR, else $XDG_DATA_HOME/cavu, else ~/.local/share/cavu)")

	return f
}

func newFlags(usage string, stderr io.Writer) verbFlags {
	fs := flag.NewFlagSet("cavu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n", usage)
		fs.PrintDefaults()
	}

	return verbFlags{FlagSet: fs}
}

// parse returns the verb's n positional arguments. They come before the flags,
// because the flag package stops at the first positional argument.
func (f verbFlags) parse(args []string, n int) ([]string, error) {
	if slices.ContainsFunc(args[:min(n, len(args))], isHelp) {
		return nil, helpError{f}
	}

	if len(args) < n {
		return nil, f.usageError()
	}

	// The flag package prints the usage itself; hold it back to tell a help
	// request from a mistake.
	usage := f.Usage
	f.Usage = func() {}
	err := f.Parse(args[n:])
	f.Usage = usage

	if errors.Is(err, flag.ErrHelp) {
		return nil, helpError{f}
	}

	if err != nil {
		f.Usage()

		return nil, exitError{code: 2, err: err}
	}

	if f.NArg() != 0 {
		return nil, f.usageError()
	}

	return args[:n], nil
}

// given returns the names of the flags set on the command line.
func (f verbFlags) given() map[string]bool {
	set := map[string]bool{}

	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	return set
}

// helpError is a request for the verb's usage with -h or --help. run prints it
// on standard output and exits 0.
type helpError struct{ f verbFlags }

func (helpError) Error() string { return "help requested" }

func isHelp(arg string) bool { return arg == "-h" || arg == "--help" }

// helpOr returns a help request when the first argument asks for one, else a usage error.
func (f verbFlags) helpOr(args []string) error {
	if len(args) > 0 && isHelp(args[0]) {
		return helpError{f}
	}

	return f.usageError()
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

// isTerminal reports whether r, a reader or writer, is a character device, as a terminal is.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
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
