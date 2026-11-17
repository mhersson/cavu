package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

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

const replayUsage = "cavu replay --name NAME [--model ID] [--effort LEVEL] [--no-continuations] [--no-verify-fix] [--image TAG] [--data-dir DIR]"

func runReplay(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(replayUsage, stderr)
	name := f.String("name", "", "the replay's name (required)")
	model := f.String("model", "", "the work model for the replay")
	effort := f.String("effort", "", "the work effort for the replay")
	noContinuations := f.Bool("no-continuations", false, "turn the continuations switch off")
	noVerifyFix := f.Bool("no-verify-fix", false, "turn the verify_fix switch off")
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

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	rt, err := a.drive(ctx, *image != "")
	if err != nil {
		return err
	}

	l := a.loop(rt)
	if *image != "" {
		l.Image = *image
	}

	if err := a.removeOrphans(ctx, rt); err != nil {
		return err
	}

	r, err := replay.Run(ctx, a.store, l, cfg, stdout)
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

	tasks, err := s.Tasks(ctx, false)
	if err != nil {
		return err
	}

	titles := map[int64]string{}
	for _, t := range tasks {
		titles[t.ID] = t.Title
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tTITLE\tVERIFY\tPREV\tTURNS\tPREV\tTOKENS\tPREV\tCOST (ESTIMATE)\tPREV\tDURATION\tPREV\tDIFF")

	for _, row := range replay.Compare(now, before, titles) {
		n, p := cells(row.Now), cells(row.Before)

		fmt.Fprintf(tw, "%d\t%s", row.SourceTaskID, row.Title)

		for i := range n[:5] {
			fmt.Fprintf(tw, "\t%s\t%s", n[i], p[i])
		}

		fmt.Fprintf(tw, "\t%s\n", n[5])
	}

	return tw.Flush()
}

// cells are an item's verify (else its stop), turns, tokens, cost, duration and
// diff path; all empty when the replay has no item for the task.
func cells(i *store.ReplayItem) [6]string {
	if i == nil {
		return [6]string{}
	}

	verify := i.Verify
	if verify == "" {
		verify = i.Stop
	}

	return [6]string{
		verify, fmt.Sprint(i.Turns), fmt.Sprint(i.Tokens), fmt.Sprintf("$%.4f", i.CostEstimateUSD),
		i.Duration.Round(time.Second).String(), i.DiffPath,
	}
}
