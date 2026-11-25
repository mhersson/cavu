package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/replay"
	"github.com/mhersson/cavu/internal/store"
)

func init() {
	commands["replay"] = command{
		summary: "replay --name NAME: re-run the marked tasks under a configuration and compare with the last replay",
		run: func(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) error {
			return runReplay(ctx, args, stdout, stderr)
		},
	}
}

const replayUsage = "cavu replay --name NAME [--model ID] [--effort LEVEL] [--no-continuations] [--no-verify-fix] [--no-readiness] [--no-review] [--web-tools] [--image TAG] [--data-dir DIR]"

func runReplay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(replayUsage, stderr)
	name := f.String("name", "", "the replay's name (required)")
	model := f.String("model", "", "the work model for the replay")
	effort := f.String("effort", "", "the work effort for the replay")
	noContinuations := f.Bool("no-continuations", false, "turn the continuations switch off")
	noVerifyFix := f.Bool("no-verify-fix", false, "turn the verify_fix switch off")
	noReadiness := f.Bool("no-readiness", false, "turn the readiness switch off")
	noReview := f.Bool("no-review", false, "turn the review switch off")
	webTools := f.Bool("web-tools", false, "turn the web_tools switch on")
	image := f.String("image", "", "the sandbox image to replay on, not checked (default: this binary's image)")

	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	if *name == "" {
		return f.usageError()
	}

	cfg := replay.Config{Name: *name, Model: *model, Effort: *effort}
	if *noContinuations {
		cfg.Continuations = new(bool)
	}

	if *noVerifyFix {
		cfg.VerifyFix = new(bool)
	}

	if *noReadiness {
		cfg.Readiness = new(bool)
	}

	if *noReview {
		cfg.Review = new(bool)
	}

	if *webTools {
		cfg.WebTools = new(bool)
		*cfg.WebTools = true
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	a.image = *image
	l := a.actions()

	var r store.Replay

	err = a.driven(ctx, func(t store.Task) bool { return t.ReplayID != 0 }, func(ctx context.Context) error {
		if a.held == nil && a.image != "" { // a server drives, under the lock it holds
			return errors.New("a cavu server drives the replay on its own image; replay without --image")
		}

		var err error

		r, err = replay.Run(ctx, a.store, l, cfg, func(ctx context.Context, id int64) (engine.Status, error) {
			return rest(ctx, l, id, stdout)
		}, stdout)

		return err
	})

	if errors.Is(err, replay.ErrNoReferenceTasks) {
		_, err = fmt.Fprintln(stdout, "no task is marked for replay with a base commit: mark one that has run with cavu task mark ID")

		return err
	}

	if ctx.Err() != nil {
		fmt.Fprintf(stderr, "interrupted; replay %q is left unended and is no comparison base\n", *name)

		return exitError{code: 130}
	}

	if err != nil {
		return err
	}

	return printReplay(ctx, a.store, r, stdout)
}

// rest waits until the task rests, saying on out why it waits, and returns its status.
func rest(ctx context.Context, l *engine.Loop, taskID int64, out io.Writer) (engine.Status, error) {
	if err := follow(ctx, l, taskID, io.Discard, out); err != nil {
		return "", err
	}

	snap, err := l.Snapshot(ctx, taskID)
	if err != nil {
		return "", err
	}

	return engine.StatusOf(snap.Facts), nil
}

// printReplay prints one row per source task: each value of this replay, then
// the previous ended replay's, empty when there is none.
func printReplay(ctx context.Context, s *store.Store, r store.Replay, w io.Writer) error {
	now, err := s.ReplayItems(ctx, r.ID)
	if err != nil {
		return err
	}

	var before []store.ReplayItem

	if prev, ok, err := s.PreviousReplay(ctx, r.ID); err != nil {
		return err
	} else if ok {
		if before, err = s.ReplayItems(ctx, prev.ID); err != nil {
			return err
		}
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tTITLE\tVERIFY\tPREV\tASKED\tPREV\tTURNS\tPREV\tTOKENS\tPREV\tCOST (ESTIMATE)\tPREV\tDURATION\tPREV\tDIFF")

	for _, row := range replay.Compare(now, before) {
		n, p := cells(row.Now), cells(row.Before)

		fmt.Fprintf(tw, "%d\t%s", row.SourceTaskID, row.Title)

		last := len(n) - 1
		for i := range n[:last] {
			fmt.Fprintf(tw, "\t%s\t%s", n[i], p[i])
		}

		fmt.Fprintf(tw, "\t%s\n", n[last])
	}

	return tw.Flush()
}

// cells are an item's verify (else its stop), questions asked, turns, tokens,
// cost, duration and diff path; all empty when the replay has no item for the task.
func cells(i *store.ReplayItem) [7]string {
	if i == nil {
		return [7]string{}
	}

	verify := i.Verify
	if verify == "" {
		verify = i.Stop
	}

	return [7]string{
		verify, fmt.Sprint(i.Asked), fmt.Sprint(i.Turns), fmt.Sprint(i.Tokens), fmt.Sprintf("$%.4f", i.CostEstimateUSD),
		i.Duration.Round(time.Second).String(), i.DiffPath,
	}
}
