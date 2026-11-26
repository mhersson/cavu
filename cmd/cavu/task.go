package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

func init() {
	commands["task"] = command{
		summary: "task add | edit | list | show | diff | run | answer | retry | cancel | done | mark: add a task, read one, run one, or act on one",
		run:     runTask,
	}
}

const (
	taskAddUsage    = "cavu task add PROJECT --title TITLE [--depends-on ID[,ID]...] [--start] [--body-file FILE, else the body is read from a pipe] [--data-dir DIR]"
	taskEditUsage   = "cavu task edit ID [--title TITLE] [--body-file FILE] [--depends-on ID[,ID]...] [--data-dir DIR]"
	taskListUsage   = "cavu task list [--needs-you] [--data-dir DIR]"
	taskShowUsage   = "cavu task show ID [--events] [--data-dir DIR]"
	taskDiffUsage   = "cavu task diff ID [--data-dir DIR]"
	taskRunUsage    = "cavu task run ID [--data-dir DIR]"
	taskAnswerUsage = "cavu task answer ID [--data-dir DIR] < FILE"
	taskRetryUsage  = "cavu task retry ID [--data-dir DIR]"
	taskCancelUsage = "cavu task cancel ID [--data-dir DIR]"
	taskDoneUsage   = "cavu task done ID [--data-dir DIR]"
	taskMarkUsage   = "cavu task mark ID [--unmark] [--data-dir DIR]"
)

func runTask(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		rest := args[1:]

		switch args[0] {
		case "add":
			return taskAdd(ctx, rest, stdin, stdout, stderr)
		case "edit":
			return taskEdit(ctx, rest, stderr)
		case "list":
			return taskList(ctx, rest, stdout, stderr)
		case "show":
			return taskShow(ctx, rest, stdout, stderr)
		case "diff":
			return taskDiff(ctx, rest, stdout, stderr)
		case "run":
			return taskRun(ctx, rest, stdout, stderr)
		case "answer":
			return taskAnswer(ctx, rest, stdin, stderr)
		case "retry":
			return taskAct(ctx, taskRetryUsage, rest, stderr, (*engine.Loop).Retry)
		case "cancel":
			return taskAct(ctx, taskCancelUsage, rest, stderr, (*engine.Loop).Cancel)
		case "done":
			return taskAct(ctx, taskDoneUsage, rest, stderr, (*engine.Loop).Done)
		case "mark":
			return taskMark(ctx, rest, stderr)
		}
	}

	usage := strings.Join([]string{
		taskAddUsage, taskEditUsage, taskListUsage, taskShowUsage, taskDiffUsage, taskRunUsage, taskAnswerUsage, taskRetryUsage, taskCancelUsage,
		taskDoneUsage, taskMarkUsage,
	}, "\n       ")

	return newVerbFlags(usage, stderr).helpOr(args)
}

// onTask parses the task id, opens the app and calls do with it.
func onTask(ctx context.Context, f verbFlags, args []string, do func(a *app, id int64) error) error {
	argv, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	id, err := strconv.ParseInt(argv[0], 10, 64)
	if err != nil {
		return fmt.Errorf("task id %q: want a number", argv[0])
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	return do(a, id)
}

func taskAdd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f := newVerbFlags(taskAddUsage, stderr)
	title := f.String("title", "", "what the task asks for, in one line of at most 200 characters")
	bodyFile := f.String("body-file", "", "a file holding the task's description")
	dependsOn := f.String("depends-on", "", "the tasks that must be done before this one runs, as ids separated by commas")
	start := f.Bool("start", false, "start the task as well")

	name, err := f.parse(args, 1)
	if err != nil {
		return err
	}

	deps, err := taskIDs(*dependsOn)
	if err != nil {
		return err
	}

	body, err := readBody(*bodyFile, stdin)
	if err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	p, err := a.store.ProjectByName(ctx, name[0])
	if err != nil {
		return fmt.Errorf("project %q: %w", name[0], err)
	}

	t, err := a.actions().AddTask(ctx, store.Task{ProjectID: p.ID, Title: *title, Body: body, Requester: "cli"}, deps, *start)
	if err != nil {
		return withBaselineHint(err, p.Name)
	}

	_, err = fmt.Fprintln(stdout, t.ID)

	return err
}

// taskEdit changes only the title, body and dependencies it is given; an empty
// --depends-on removes every dependency.
func taskEdit(ctx context.Context, args []string, stderr io.Writer) error {
	f := newVerbFlags(taskEditUsage, stderr)
	title := f.String("title", "", "what the task asks for, in one line of at most 200 characters")
	bodyFile := f.String("body-file", "", "a file holding the task's description")
	dependsOn := f.String("depends-on", "", "the tasks that must be done before this one runs, as ids separated by commas; empty removes them all")

	return onTask(ctx, f, args, func(a *app, id int64) error {
		given := f.given()

		var e store.TaskEdit

		if given["title"] {
			e.Title = title
		}

		if given["body-file"] {
			body, err := readBody(*bodyFile, nil)
			if err != nil {
				return err
			}

			e.Body = &body
		}

		if given["depends-on"] {
			deps, err := taskIDs(*dependsOn)
			if err != nil {
				return err
			}

			e.DependsOn = &deps
		}

		return engine.EditTask(ctx, a.store, id, e)
	})
}

// taskIDs reads task ids separated by commas; empty is none.
func taskIDs(list string) ([]int64, error) {
	if list == "" {
		return nil, nil
	}

	var ids []int64

	for field := range strings.SplitSeq(list, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("task ids %q: want numbers separated by commas", list)
		}

		ids = append(ids, id)
	}

	return ids, nil
}

// startTask records that a person started the task, naming the verbs that
// give its project a baseline when it has none.
func startTask(ctx context.Context, l *engine.Loop, id int64, project string) error {
	return withBaselineHint(l.Start(ctx, id), project)
}

func withBaselineHint(err error, project string) error {
	if errors.Is(err, engine.ErrNoBaseline) {
		return fmt.Errorf("%w; run cavu project check %s, or waive verify with cavu project set %s --no-verify", err, project, project)
	}

	return err
}

// readBody reads the file, else a pipe; typed on a terminal, the task has no body.
func readBody(file string, stdin io.Reader) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)

		return string(b), err
	}

	if isTerminal(stdin) {
		return "", nil
	}

	b, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("standard input: %w", err)
	}

	return string(b), nil
}

func taskList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(taskListUsage, stderr)
	needsYou := f.Bool("needs-you", false, "list only the tasks that wait for a person")

	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	rows, err := a.actions().List(ctx, engine.ListQuery{NeedsYou: *needsYou}, nil)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPROJECT\tSTATUS\tNEEDS YOU\tTITLE")

	for _, r := range rows {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.ID, r.Project, r.Status, r.Needs, r.Title)
	}

	return w.Flush()
}

func taskShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(taskShowUsage, stderr)
	withEvents := f.Bool("events", false, "list every event of the task")

	return onTask(ctx, f, args, func(a *app, id int64) error {
		var pauses engine.Pauses
		if err := pauses.Read(ctx, a.store); err != nil {
			return err
		}

		l := a.actions()
		l.Paused = pauses.Of

		v, err := l.View(ctx, id, nil)
		if err != nil {
			return err
		}

		printView(stdout, v)

		if !*withEvents {
			return nil
		}

		all, err := a.store.Events(ctx, id, 0, 0)
		if err != nil {
			return err
		}

		printEvents(stdout, all)

		return nil
	})
}

// printView prints what a person reads about the task: its status, whether it
// needs you, its open stop with every question the agent asked and the answer
// it suggested, its pull request, the review's findings left open, its minor
// ones and why its newest review gave no verdict, the change it shows with
// git's summary of it and its flagged paths, its dependencies and wait, and
// what it has used.
func printView(w io.Writer, v engine.View) {
	fmt.Fprintf(w, "task:    %d\nproject: %s\ntitle:   %s\nstatus:  %s\n", v.ID, v.Project, v.Title, v.Status)

	if v.Needs != "" {
		fmt.Fprintf(w, "needs:   you: %s (since %s)\n", v.Needs, v.NeedsYouSince.Local().Format(time.DateTime))
	}

	if s := v.Stop; s != nil {
		fmt.Fprintf(w, "stop:    %s\nwhy:     %s\naction:  %s\n", s.Reason, s.Explanation, s.Action)
	}

	for _, q := range v.Questions {
		fmt.Fprintf(w, "asked:   %s\n", q.Question)

		if q.SuggestedAnswer != "" {
			fmt.Fprintf(w, "suggest: %s\n", q.SuggestedAnswer)
		}
	}

	if pr := v.PullRequest; pr != nil {
		state := pr.State
		if pr.Ready {
			state += ", ready for your review"
		}

		fmt.Fprintf(w, "pr:      %s (%s)\nchecks:  %s\n", pr.URL, state, engine.ChecksText(pr.Checks, pr.Failed))
	}

	if r := v.Review; r != nil {
		for _, f := range r.Open {
			fmt.Fprintf(w, "open:    %s\n", findingLine(f))
		}

		for _, f := range r.Minor {
			fmt.Fprintf(w, "minor:   %s\n", findingLine(f))
		}

		if r.Failed != "" {
			fmt.Fprintf(w, "review:  no verdict: %s\n", r.Failed)
		}
	}

	if x := v.Change; x != nil {
		fmt.Fprintf(w, "diff:    %s\n", x.DiffPath)

		for _, l := range x.DiffStat {
			fmt.Fprintf(w, "changed: %s\n", l)
		}

		fmt.Fprintf(w, "bundle:  %s\n", x.BundlePath)

		for _, k := range []struct {
			kind  string
			paths []string
		}{
			{"workflows", x.Flagged.Workflows}, {"agent instructions", x.Flagged.Instructions}, {"large files added", x.Flagged.Large},
		} {
			if len(k.paths) > 0 {
				fmt.Fprintf(w, "flagged: %s: %s\n", k.kind, strings.Join(k.paths, ", "))
			}
		}
	}

	if len(v.Dependencies) > 0 {
		deps := make([]string, len(v.Dependencies))
		for i, d := range v.Dependencies {
			deps[i] = fmt.Sprintf("%d (%s)", d.TaskID, d.Status)
		}

		fmt.Fprintf(w, "depends: %s\n", strings.Join(deps, ", "))
	}

	if v.Waiting != "" {
		var until time.Time
		if v.WaitingUntil != nil {
			until = *v.WaitingUntil
		}

		fmt.Fprintf(w, "wait:    %s\n", waitText(v.Waiting, until))
	}

	b := v.Budget

	fmt.Fprintf(w, "turns:   %d\nduration: %s\n", v.Turns, (time.Duration(v.DurationMS) * time.Millisecond).Round(time.Second))
	fmt.Fprintf(w, "budget:  %d of %d turns, %d of %d minutes", b.Turns, b.MaxTurns, b.Minutes, b.MaxMinutes)

	// The money of a cap is there only while that cap is in force.
	if b.USD != nil {
		fmt.Fprintf(w, ", $%.2f of $%.2f", *b.USD, *b.MaxUSD)
	}

	fmt.Fprint(w, " since a person last acted")

	if b.DayUSD != nil {
		fmt.Fprintf(w, "; $%.2f of the daily $%.2f today", *b.DayUSD, *b.MaxDayUSD)
	}

	fmt.Fprintln(w)

	for _, u := range v.Usage {
		fmt.Fprintf(w, "usage:   %s %s input=%d output=%d cache_read=%d cache_write=%d\n", u.Source, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite)
	}

	// An estimate from list prices, not a bill, that leaves out the sessions without a cost.
	fmt.Fprintf(w, "cost (estimate): $%.4f", v.CostEstimateUSD)

	if n := v.CostMissingSessions; n > 0 {
		noun := "sessions"
		if n == 1 {
			noun = "session"
		}

		fmt.Fprintf(w, ", leaving out %d %s that ended without one", n, noun)
	}

	fmt.Fprintln(w)
}

// findingLine is a finding's severity, where, what and why, each that is set.
func findingLine(f engine.Finding) string {
	return strings.Join(slices.DeleteFunc([]string{f.Severity, f.Where, f.What, f.Why}, func(s string) bool { return s == "" }), " | ")
}

func printEvents(w io.Writer, events []store.Event) {
	fmt.Fprintln(w, "events:")

	for _, e := range events {
		fmt.Fprintf(w, "  %d %s %s %s %s\n", e.ID, e.At.Format(time.RFC3339), e.Type, e.Actor, e.Payload)
	}
}

func taskDiff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return onTask(ctx, newVerbFlags(taskDiffUsage, stderr), args, func(a *app, id int64) error {
		events, err := a.store.EventsOmitting(ctx, id, engine.ProgressTypes...)
		if err != nil {
			return err
		}

		x := engine.ChangeOf(engine.Reduce(events))
		if x == nil {
			return fmt.Errorf("task %d has no exported change yet", id)
		}

		diff, err := os.ReadFile(x.DiffPath)
		if err != nil {
			return err
		}

		_, err = stdout.Write(diff)

		return err
	})
}

// taskRun starts the task once something drives it, and prints its events
// until it rests, then its state. The watch of a pull request is cavu
// serve's: the verb ends at it, and returns at once on a task already there.
func taskRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return onTask(ctx, newVerbFlags(taskRunUsage, stderr), args, func(a *app, id int64) error {
		t, err := a.store.Task(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		p, err := a.store.Project(ctx, t.ProjectID)
		if err != nil {
			return err
		}

		l := a.actions()

		snap, err := l.Snapshot(ctx, id)
		if err != nil {
			return err
		}

		if engine.Decide(snap).Kind == engine.KindWatch {
			return ended(ctx, stdout, l, id, true)
		}

		err = a.driven(ctx, func(t store.Task) bool { return t.ID == id }, func(ctx context.Context) error {
			if err := startTask(ctx, l, id, p.Name); err != nil {
				return err
			}

			return follow(ctx, l, id, stdout, stdout)
		})

		if ctx.Err() != nil {
			if err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(stderr, "cavu: %v\n", err)
			}

			if a.held != nil { // this verb held the lock, so it drove the task itself
				fmt.Fprintf(stderr, "interrupted; cavu task run %d carries on where this run stopped\n", id)
			} else {
				fmt.Fprintf(stderr, "interrupted; the server carries on with task %d\n", id)
			}

			return exitError{code: 130}
		}

		snap, serr := l.Snapshot(ctx, id)
		if serr != nil {
			return errors.Join(err, serr)
		}

		// The scheduler's look at a pull request that just opened can fail, with
		// GitHub down or rate limited: told, it ends the verb as the watch does.
		watched := engine.Decide(snap).Kind == engine.KindWatch
		if err != nil && !watched {
			return err
		}

		if err != nil {
			fmt.Fprintf(stderr, "cavu: %v\n", err)
		}

		return ended(ctx, stdout, l, id, watched)
	})
}

// ended prints the task as task show does where task run ends, and when
// Decide watches its pull request, that cavu serve does. A stopped task exits 3.
func ended(ctx context.Context, w io.Writer, l *engine.Loop, id int64, watched bool) error {
	v, err := l.View(ctx, id, nil)
	if err != nil {
		return err
	}

	printView(w, v)

	if watched {
		fmt.Fprintln(w, "cavu serve watches the pull request from here; task run does not.")
	}

	if v.Status == engine.StatusBlocked || v.Status == engine.StatusCancelled {
		return exitError{code: 3}
	}

	return nil
}

// follow prints each new event of the task as one line on events, and each
// wait's reason once on waits, until the task rests. It begins at the newest
// action of a person, which is what it carries on from. It gives l the pauses
// the store shows, as a scheduler would, so it decides alike beside a
// scheduler or a server.
func follow(ctx context.Context, l *engine.Loop, taskID int64, events, waits io.Writer) error {
	var (
		pauses engine.Pauses
		after  int64
		waited string
	)

	l.Paused = pauses.Of

	tick := time.NewTicker(pollEvery)
	defer tick.Stop()

	for {
		if err := pauses.Read(ctx, l.Store); err != nil {
			return err
		}

		snap, err := l.Snapshot(ctx, taskID)
		if err != nil {
			return err
		}

		// Read after the snapshot, so the events it decided on are all printed.
		evs, err := l.Store.Events(ctx, taskID, after, 0)
		if err != nil {
			return err
		}

		if after == 0 {
			for i, e := range slices.Backward(evs) {
				if e.Actor == "user" {
					evs = evs[i:]

					break
				}
			}
		}

		for _, e := range evs {
			fmt.Fprintln(events, eventLine(e))

			after = e.ID
		}

		switch act := engine.Decide(snap); {
		case act.Kind == engine.KindNone || act.Kind == engine.KindWatch:
			return nil
		case act.Kind != engine.KindWait:
			waited = ""
		case waitText(act.Why, act.Until) != waited:
			waited = waitText(act.Why, act.Until)
			fmt.Fprintf(waits, "%s wait %s\n", snap.Now.Format(time.TimeOnly), waited)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// waitText is a wait's reason, with its end when a clock ends it.
func waitText(why string, until time.Time) string {
	if until.IsZero() {
		return why
	}

	return fmt.Sprintf("%s (until %s)", why, until.Local().Format(time.DateTime))
}

// maxLine bounds an event's line, in bytes.
const maxLine = 200

// eventLine is an event on one line: its time, its type and its sentence.
func eventLine(e store.Event) string {
	l := strings.Join(strings.Fields(e.At.Format(time.TimeOnly)+" "+e.Type+" "+engine.EventText(e)), " ")

	// Output that is no terminal is left byte-exact, so the line shows the
	// agent's control characters itself, as a terminal's output does.
	var b strings.Builder

	_, _ = (&visibleWriter{w: &b}).Write([]byte(l))

	if l = b.String(); len(l) <= maxLine {
		return l
	}

	n := maxLine - len("...")
	for !utf8.RuneStart(l[n]) {
		n--
	}

	return l[:n] + "..."
}

func taskAnswer(ctx context.Context, args []string, stdin io.Reader, stderr io.Writer) error {
	return onTask(ctx, newVerbFlags(taskAnswerUsage, stderr), args, func(a *app, id int64) error {
		if isTerminal(stdin) {
			fmt.Fprintln(stderr, "type the answer, then end the input (Ctrl-D)")
		}

		text, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("standard input: %w", err)
		}

		return a.actions().Answer(ctx, id, string(text))
	})
}

// taskAct runs a human action that takes the task id alone.
func taskAct(ctx context.Context, usage string, args []string, stderr io.Writer, act func(*engine.Loop, context.Context, int64) error) error {
	return onTask(ctx, newVerbFlags(usage, stderr), args, func(a *app, id int64) error {
		return act(a.actions(), ctx, id)
	})
}

func taskMark(ctx context.Context, args []string, stderr io.Writer) error {
	f := newVerbFlags(taskMarkUsage, stderr)
	unmark := f.Bool("unmark", false, "clear the mark")

	return onTask(ctx, f, args, func(a *app, id int64) error {
		t, err := a.store.Task(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		t.Reference = !*unmark

		return a.store.UpdateTask(ctx, t)
	})
}
