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
	"sync"
	"syscall"
	"time"

	"github.com/mhersson/cavu/internal/api"
	"github.com/mhersson/cavu/internal/auth"
	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/push"
	"github.com/mhersson/cavu/web"
)

func init() {
	commands["serve"] = command{
		summary: "serve: drive the tasks and serve the web app and the HTTP API until interrupted",
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

	bundle, err := a.caBundle(ctx)
	if err != nil {
		return err
	}

	client, err := push.NewClient(bundle)
	if err != nil {
		return fmt.Errorf("ca_bundle_path: %w", err)
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

	return a.serve(ctx, ln, rt, client, stdout)
}

// serve runs the scheduler, the notifier and the handler on ln until ctx ends
// or the handler stops serving. The notifier posts with client.
func (a *app) serve(ctx context.Context, ln net.Listener, rt containerRuntime, client *http.Client, stdout io.Writer) error {
	s := &engine.Scheduler{Loop: a.loop(rt), Checks: true, OnError: func(taskID int64, err error) {
		slog.Error("scheduler error", "task", taskID, "err", err)
	}}
	l := a.loop(rt)
	l.Paused = s.Paused
	focus := &push.Focus{}
	n := &push.Notifier{
		List: func(ctx context.Context) ([]engine.Row, error) {
			return l.List(ctx, engine.ListQuery{NeedsYou: true}, nil)
		},
		Store:   a.store,
		Sender:  &push.Sender{Client: client, Signer: push.NewSigner(a.pushKey, time.Now)},
		Focus:   focus,
		OnError: func(err error) { slog.Error("notifier error", "err", err) },
	}
	stopped := make(chan struct{})
	srv := &http.Server{
		Handler: web.Handler(api.New(api.Deps{
			Store: a.store, Vault: a.vault, Auth: auth.NewService(a.store, time.Now), Loop: l,
			Nudge: s.Nudge, Waiting: s.Waiting, CheckStarted: s.CheckStarted, Version: version, Stop: stopped,
			Focus: focus, PushKey: push.PublicKey(a.pushKey),
		})),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Shutdown waits for the open requests, and a stream ends only when told.
	srv.RegisterOnShutdown(func() { close(stopped) })

	// The scheduler ends after the open requests, which may still record a person's action.
	sctx, stop := context.WithCancel(context.WithoutCancel(ctx))

	var ran sync.WaitGroup

	ran.Go(func() { _ = s.Run(sctx) })
	ran.Go(func() { _ = n.Run(sctx) })

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
	ran.Wait()
	slog.Info("stopped")

	return err
}
