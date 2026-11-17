package main

import (
	"context"
	"encoding/json"
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
		summary: "task add | list | show | diff | run | answer | retry | cancel | mark: add a task, read one, run one, or act on one",
		run:     runTask,
	}
}

const (
	taskAddUsage    = "cavu task add PROJECT --title TITLE [--body-file FILE, else the body is read from a pipe] [--data-dir DIR]"
	taskListUsage   = "cavu task list [--data-dir DIR]"
	taskShowUsage   = "cavu task show ID [--events] [--data-dir DIR]"
	taskDiffUsage   = "cavu task diff ID [--data-dir DIR]"
	taskRunUsage    = "cavu task run ID [--data-dir DIR]"
	taskAnswerUsage = "cavu task answer ID [--data-dir DIR] < FILE"
	taskRetryUsage  = "cavu task retry ID [--data-dir DIR]"
	taskCancelUsage = "cavu task cancel ID [--data-dir DIR]"
	taskMarkUsage   = "cavu task mark ID [--unmark] [--data-dir DIR]"
)

func runTask(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		rest := args[1:]

		switch args[0] {
		case "add":
			return taskAdd(ctx, rest, stdin, stdout, stderr)
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
		case "mark":
			return taskMark(ctx, rest, stderr)
		}
	}

	usage := strings.Join([]string{
		taskAddUsage, taskListUsage, taskShowUsage, taskDiffUsage, taskRunUsage, taskAnswerUsage, taskRetryUsage, taskCancelUsage, taskMarkUsage,
	}, "\n       ")

	return newVerbFlags(usage, stderr).usageError()
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

	name, err := f.parse(args, 1)
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

	t, err := engine.AddTask(ctx, a.store, store.Task{ProjectID: p.ID, Title: *title, Body: body, Requester: "cli"})
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(stdout, t.ID)

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
	if _, err := f.parse(args, 0); err != nil {
		return err
	}

	a, err := f.open(ctx)
	if err != nil {
		return err
	}
	defer a.close()

	ps, err := a.store.Projects(ctx)
	if err != nil {
		return err
	}

	names := map[int64]string{}
	for _, p := range ps {
		names[p.ID] = p.Name
	}

	tasks, err := a.store.Tasks(ctx, false)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPROJECT\tSTATUS\tTITLE")

	for _, t := range tasks {
		events, err := a.store.Events(ctx, t.ID, 0, 0)
		if err != nil {
			return err
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", t.ID, names[t.ProjectID], engine.StatusOf(engine.Reduce(events)), t.Title)
	}

	return w.Flush()
}

func taskShow(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	f := newVerbFlags(taskShowUsage, stderr)
	withEvents := f.Bool("events", false, "list every event of the task")

	return onTask(ctx, f, args, func(a *app, id int64) error {
		t, err := a.store.Task(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		p, err := a.store.Project(ctx, t.ProjectID)
		if err != nil {
			return err
		}

		events, err := a.store.Events(ctx, id, 0, 0)
		if err != nil {
			return err
		}

		fmt.Fprintf(stdout, "task:    %d\nproject: %s\ntitle:   %s\n", t.ID, p.Name, t.Title)
		printState(stdout, events)

		if err := printUsage(ctx, stdout, a.store, id); err != nil {
			return err
		}

		if *withEvents {
			printEvents(stdout, events)
		}

		return nil
	})
}

// printState prints the task's status, its open stop and its newest export,
// and returns the status.
func printState(w io.Writer, events []store.Event) engine.Status {
	facts := engine.Reduce(events)
	status := engine.StatusOf(facts)

	fmt.Fprintf(w, "status:  %s\n", status)

	if facts.OpenStop != nil {
		s, _ := newest[engine.StoppedPayload](events, engine.Stopped)
		fmt.Fprintf(w, "stop:    %s\nwhy:     %s\naction:  %s\n", s.Reason, s.Explanation, s.Action)
	}

	if x, ok := newest[engine.ExportedPayload](events, engine.Exported); ok {
		fmt.Fprintf(w, "diff:    %s\nbundle:  %s\n", x.DiffPath, x.BundlePath)
	}

	return status
}

// newest decodes the newest event of the type, if any.
func newest[T any](events []store.Event, typ string) (T, bool) {
	var v T

	for _, e := range slices.Backward(events) {
		if e.Type == typ {
			return v, json.Unmarshal(e.Payload, &v) == nil
		}
	}

	return v, false
}

// printUsage prints a row per source and model and the one cost row, which
// is an estimate from list prices, not a bill.
func printUsage(ctx context.Context, w io.Writer, s *store.Store, taskID int64) error {
	rows, err := s.UsageTotals(ctx, taskID)
	if err != nil {
		return err
	}

	var cost float64

	for _, u := range rows {
		fmt.Fprintf(w, "usage:   %s %s input=%d output=%d cache_read=%d cache_write=%d\n",
			u.Source, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite)

		cost += u.CostEstimateUSD
	}

	fmt.Fprintf(w, "cost (estimate): $%.4f\n", cost)

	return nil
}

func printEvents(w io.Writer, events []store.Event) {
	fmt.Fprintln(w, "events:")

	for _, e := range events {
		fmt.Fprintf(w, "  %d %s %s %s %s\n", e.ID, e.At.Format(time.RFC3339), e.Type, e.Actor, e.Payload)
	}
}

func taskDiff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return onTask(ctx, newVerbFlags(taskDiffUsage, stderr), args, func(a *app, id int64) error {
		events, err := a.store.Events(ctx, id, 0, 0)
		if err != nil {
			return err
		}

		x, ok := newest[engine.ExportedPayload](events, engine.Exported)
		if !ok {
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

// releaseTimeout bounds stopping an interrupted run's containers.
const releaseTimeout = time.Minute

// taskRun starts the task, removes the instance's orphans, and runs the loop
// until the task rests, printing each event as it is recorded.
func taskRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return onTask(ctx, newVerbFlags(taskRunUsage, stderr), args, func(a *app, id int64) error {
		rt, err := a.drive(ctx)
		if err != nil {
			return err
		}

		t, err := a.store.Task(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		p, err := a.store.Project(ctx, t.ProjectID)
		if err != nil {
			return err
		}

		l := a.loop(rt)
		l.Store = printingStore{Store: l.Store, out: stdout}

		err = l.Start(ctx, id)
		if errors.Is(err, engine.ErrNoBaseline) {
			return fmt.Errorf("%w; run cavu project check %s, or waive verify with cavu project set %s --no-verify", err, p.Name, p.Name)
		}

		if err != nil {
			return err
		}

		if err := a.removeOrphans(ctx, rt); err != nil {
			return err
		}

		_, err = l.RunTask(ctx, id)

		if ctx.Err() != nil {
			if err := a.releaseOpenRun(ctx, l, t); err != nil {
				fmt.Fprintf(stderr, "cavu: %v\n", err)
			}

			fmt.Fprintf(stderr, "interrupted; cavu task run %d carries on where this run stopped\n", id)

			return exitError{code: 130}
		}

		if err != nil {
			return err
		}

		events, err := a.store.Events(ctx, id, 0, 0)
		if err != nil {
			return err
		}

		if s := printState(stdout, events); s == engine.StatusBlocked || s == engine.StatusCancelled {
			return exitError{code: 3}
		}

		return nil
	})
}

// releaseOpenRun stops the containers of the task's open run and keeps its
// volumes, on a context that outlives the interrupt that ended ctx.
func (a *app) releaseOpenRun(ctx context.Context, l *engine.Loop, t store.Task) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	row, ok, err := a.store.OpenRun(ctx, t.ID)
	if err != nil || !ok {
		return err
	}

	return l.Runner.Release(ctx, engine.RunRef{
		InstanceID: a.instanceID, Image: l.Image, TaskID: t.ID, RunID: row.ID, ProjectID: t.ProjectID,
	}, false)
}

// printingStore prints each event once its append succeeded. The engine's
// write masked every payload before the append, so nothing here masks again.
type printingStore struct {
	engine.Store

	out io.Writer
}

func (s printingStore) Append(ctx context.Context, taskID, expectLast int64, events []store.Event, usage []store.Usage) (int64, error) {
	last, err := s.Store.Append(ctx, taskID, expectLast, events, usage)
	if err == nil {
		for _, e := range events {
			fmt.Fprintf(s.out, "%s %s %s\n", e.At.Format(time.TimeOnly), e.Type, summary(e.Payload))
		}
	}

	return last, err
}

// summary is the head of a payload, short enough for one line on a terminal.
// The payload is JSON, which escapes line breaks and control characters.
func summary(payload []byte) string {
	const n = 80

	if len(payload) <= n {
		return string(payload)
	}

	i := n
	for i > 0 && !utf8.RuneStart(payload[i]) {
		i--
	}

	return string(payload[:i]) + "..."
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
