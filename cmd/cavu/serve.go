package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mhersson/cavu/internal/api"
	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
)

func init() {
	commands["serve"] = command{
		summary: "serve: drive the tasks and serve the HTTP API until interrupted",
		run:     runServe,
	}
}

// shutdownGrace is how long open requests get once the server stops.
const shutdownGrace = 5 * time.Second

func runServe(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	f := newVerbFlags("cavu serve [--data-dir DIR]", stderr)
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	if err := a.lock(roleServe); a.serverHolds(err) {
		return fmt.Errorf("a cavu server is already running on %s", a.dir)
	} else if errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("a cavu %s holds %s; wait for it to end", roleVerb, filepath.Join(a.dir, "lock"))
	} else if err != nil {
		return err
	}

	settings, err := a.store.Settings(ctx)
	if err != nil {
		return err
	}

	addr, err := engine.ListenAddress(settings)
	if err != nil {
		return err
	}

	rt, err := a.runtime(ctx)
	if err != nil {
		return err
	}

	if err := a.checkImage(ctx, rt); err != nil {
		return err
	}

	if err := a.removeOrphans(ctx, rt); err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, nil)))

	if _, found, err := a.store.User(ctx); err != nil {
		return err
	} else if !found {
		slog.Warn("no user exists yet, so nobody can log in; set one with cavu user set NAME < FILE")
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}

	return a.serve(ctx, ln, rt, stdout)
}

// serve runs the scheduler and the handler on ln until ctx ends or the
// handler stops serving.
func (a *app) serve(ctx context.Context, ln net.Listener, rt containerRuntime, stdout io.Writer) error {
	s := &engine.Scheduler{Loop: a.loop(rt), Checks: true, OnError: func(taskID int64, err error) {
		slog.Error("scheduler error", "task", taskID, "err", err)
	}}
	l := a.loop(rt)
	l.Paused = s.Paused
	stopped := make(chan struct{})
	srv := &http.Server{
		Handler: api.New(api.Deps{
			Store: a.store, Vault: a.vault, Auth: auth.NewService(a.store, time.Now), Loop: l,
			Nudge: s.Nudge, Waiting: s.Waiting, Version: version, Stop: stopped,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Shutdown waits for the open requests, and a stream ends only when told.
	srv.RegisterOnShutdown(func() { close(stopped) })

	// The scheduler ends after the open requests, which may still record a person's action.
	sctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	ran := make(chan struct{})

	go func() {
		defer close(ran)

		_ = s.Run(sctx)
	}()

	served := make(chan error, 1)

	go func() { served <- srv.Serve(ln) }()

	slog.Info("started", "version", version, "address", ln.Addr().String(), "data_dir", a.dir)
	fmt.Fprintf(stdout, "listening on http://%s\n", ln.Addr())

	var err error

	select {
	case <-ctx.Done():
		gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		if serr := srv.Shutdown(gctx); serr != nil {
			slog.Warn("cut the requests still open", "err", serr)

			_ = srv.Close()
		}

		cancel()
	case err = <-served:
	}

	stop()
	<-ran
	slog.Info("stopped")

	return err
}
