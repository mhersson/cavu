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
		summary: "task add | edit | list | show | diff | run | answer | retry | cancel | done | mark: add a task, read one, run one, or act on one",
		run:     runTask,
	}
}

const (
	taskAddUsage    = "cavu task add PROJECT --title TITLE [--depends-on ID[,ID]...] [--start] [--body-file FILE, else the body is read from a pipe] [--data-dir DIR]"
	taskEditUsage   = "cavu task edit ID [--title TITLE] [--body-file FILE] [--depends-on ID[,ID]...] [--data-dir DIR]"
	taskListUsage   = "cavu task list [--data-dir DIR]"
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

	t, err := engine.AddTask(ctx, a.store, store.Task{ProjectID: p.ID, Title: *title, Body: body, Requester: "cli"}, deps)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintln(stdout, t.ID); err != nil || !*start {
		return err
	}

	return startTask(ctx, a.actions(), t.ID, p.Name)
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

		t, err := a.store.Task(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		deps, err := a.store.TaskDeps(ctx, id)
		if err != nil {
			return fmt.Errorf("task %d: %w", id, err)
		}

		if given["title"] {
			t.Title = *title
		}

		if given["body-file"] {
			if t.Body, err = readBody(*bodyFile, nil); err != nil {
				return err
			}
		}

		if given["depends-on"] {
			if deps, err = taskIDs(*dependsOn); err != nil {
				return err
			}
		}

		return engine.EditTask(ctx, a.store, t, deps)
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
	err := l.Start(ctx, id)
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
		events, err := a.store.EventsOmitting(ctx, t.ID, engine.ProgressTypes...)
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

		events, err := a.store.EventsOmitting(ctx, id, engine.ProgressTypes...)
		if err != nil {
			return err
		}

		var pauses engine.Pauses
		if err := pauses.Read(ctx, a.store); err != nil {
			return err
		}

		l := a.actions()
		l.Paused = pauses.Of

		snap, err := l.Snapshot(ctx, id)
		if err != nil {
			return err
		}

		fmt.Fprintf(stdout, "task:    %d\nproject: %s\ntitle:   %s\n", t.ID, p.Name, t.Title)
		printState(stdout, snap)
		printQueue(stdout, snap)

		turns, d := engine.TurnsAndDuration(events)
		fmt.Fprintf(stdout, "turns:   %d\nduration: %s\n", turns, d.Round(time.Second))

		used, limits := snap.Facts, snap.Limits
		spent, today := money(snap)
		fmt.Fprintf(stdout, "budget:  %d of %d turns, %.0f of %.0f minutes%s since a person last acted%s\n",
			used.Turns, limits.Turns, used.SessionTime.Minutes(), limits.SessionTime.Minutes(), spent, today)

		if err := printUsage(ctx, stdout, a.store, id, used.CostMissing); err != nil {
			return err
		}

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

// money is what the budget line says of the money caps in force: the task's
// spend of its cap, and the instance's today of the daily cap.
func money(s engine.Snapshot) (task, day string) {
	if c := s.Limits.TaskMicroUSD; c > 0 {
		task = fmt.Sprintf(", %s of %s", dollars(s.Spent.TaskMicroUSD), dollars(c))
	}

	if c := s.Limits.DayMicroUSD; c > 0 {
		day = fmt.Sprintf("; %s of the daily %s today", dollars(s.Spent.DayMicroUSD), dollars(c))
	}

	return task, day
}

func dollars(microUSD int64) string {
	return fmt.Sprintf("$%.2f", float64(microUSD)/1e6)
}

// printQueue prints the task's dependencies, each with its status, and why
// Decide holds the task, or watches its pull request, while it does.
func printQueue(w io.Writer, snap engine.Snapshot) {
	if len(snap.Deps) > 0 {
		deps := make([]string, len(snap.Deps))
		for i, d := range snap.Deps {
			deps[i] = fmt.Sprintf("%d (%s)", d.TaskID, d.Status)
		}

		fmt.Fprintf(w, "depends: %s\n", strings.Join(deps, ", "))
	}

	if act := engine.Decide(snap); act.Kind == engine.KindWait || act.Kind == engine.KindWatch {
		fmt.Fprintf(w, "wait:    %s\n", waitText(act))
	}
}

// printState prints the task's status, its open stop with every question the
// agent asked and the answer it suggested, its pull request, the review's
// findings left open, its minor ones and why its newest review gave no
// verdict, and the change it shows with git's summary of it and its flagged
// paths, and returns the status.
func printState(w io.Writer, s engine.Snapshot) engine.Status {
	facts := s.Facts
	status := engine.StatusOf(facts)

	fmt.Fprintf(w, "status:  %s\n", status)

	if facts.OpenStop != nil {
		s := facts.OpenStop
		fmt.Fprintf(w, "stop:    %s\nwhy:     %s\naction:  %s\n", s.Reason, s.Explanation, s.Action)

		// The stop's explanation holds only the first question.
		if facts.OpenStop.Reason == engine.StopQuestion && facts.Session != nil {
			suggested := facts.Session.SuggestedAnswers

			for i, q := range facts.Session.Questions {
				fmt.Fprintf(w, "asked:   %s\n", q)

				if i < len(suggested) && suggested[i] != "" {
					fmt.Fprintf(w, "suggest: %s\n", suggested[i])
				}
			}
		}
	}

	if pr := engine.PullRequestOf(facts, s.Watch.NoCI); pr != nil {
		state := pr.State
		if pr.Ready {
			state += ", ready for your review"
		}

		fmt.Fprintf(w, "pr:      %s (%s)\nchecks:  %s\n", pr.URL, state, checksText(pr.Checks, pr.Failed))
	}

	for _, f := range facts.Review.Open {
		fmt.Fprintf(w, "open:    %s\n", findingLine(f))
	}

	for _, f := range facts.Review.Minor {
		fmt.Fprintf(w, "minor:   %s\n", findingLine(f))
	}

	if facts.Review.Failed != "" {
		fmt.Fprintf(w, "review:  no verdict: %s\n", facts.Review.Failed)
	}

	if x := engine.ChangeOf(facts); x != nil {
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

	return status
}

// checksText is a pull request's checks, the failed ones named.
func checksText(checks string, failed []string) string {
	switch {
	case checks == "":
		return "not read yet"
	case len(failed) == 0:
		return checks
	}

	return checks + ": " + strings.Join(failed, ", ")
}

// findingLine is a finding's severity, where, what and why, each that is set.
func findingLine(f engine.Finding) string {
	return strings.Join(slices.DeleteFunc([]string{f.Severity, f.Where, f.What, f.Why}, func(s string) bool { return s == "" }), " | ")
}

// printUsage prints a row per source and model and the one cost row, an
// estimate from list prices, not a bill, which ends with how many sessions
// without a cost it leaves out, when any.
func printUsage(ctx context.Context, w io.Writer, s *store.Store, taskID int64, missing int) error {
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

	fmt.Fprintf(w, "cost (estimate): $%.4f", cost)

	if missing > 0 {
		noun := "sessions"
		if missing == 1 {
			noun = "session"
		}

		fmt.Fprintf(w, ", leaving out %d %s that ended without one", missing, noun)
	}

	fmt.Fprintln(w)

	return nil
}

// counted returns n with noun, pluralised unless n is 1.
func counted(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}

	return fmt.Sprintf("%d %ss", n, noun)
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
			return ended(stdout, snap, true)
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

		return ended(stdout, snap, watched)
	})
}

// ended prints the task's state where task run ends, and when Decide watches
// its pull request, that cavu serve does. A stopped task exits 3.
func ended(w io.Writer, s engine.Snapshot, watched bool) error {
	status := printState(w, s)

	if watched {
		fmt.Fprintln(w, "cavu serve watches the pull request from here; task run does not.")
	}

	if status == engine.StatusBlocked || status == engine.StatusCancelled {
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
		case waitText(act) != waited:
			waited = waitText(act)
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
func waitText(act engine.Action) string {
	if act.Until.IsZero() {
		return act.Why
	}

	return fmt.Sprintf("%s (until %s)", act.Why, act.Until.Local().Format(time.DateTime))
}

// maxLine bounds an event's line, in bytes.
const maxLine = 200

// eventLine is an event on one line: its time, its type and its telling field.
func eventLine(e store.Event) string {
	var tell string

	switch e.Type {
	case engine.AgentText:
		tell = decoded[engine.AgentTextPayload](e.Payload).Text
	case engine.AgentTool:
		p := decoded[engine.AgentToolPayload](e.Payload)
		tell = p.Name + " " + p.Summary
	case engine.SessionStarted:
		tell = decoded[engine.SessionStartedPayload](e.Payload).Mode
	case engine.SessionEnded:
		p := decoded[engine.SessionEndedPayload](e.Payload)
		tell = fmt.Sprintf("%s after %s", p.Outcome, counted(p.Turns, "turn"))
	case engine.ReviewStarted:
		tell = "the whole change"
		if decoded[engine.ReviewStartedPayload](e.Payload).Scoped {
			tell = "the fix and the findings it was to address"
		}
	case engine.ReviewEnded:
		p := decoded[engine.ReviewEndedPayload](e.Payload)
		tell = fmt.Sprintf("%s after %s with %s", p.Outcome, counted(p.Turns, "turn"), counted(len(p.Findings), "finding"))

		if p.Scoped && p.Outcome == engine.ReviewReviewed {
			addressed := 0

			for _, a := range p.Addressed {
				if a {
					addressed++
				}
			}

			tell += fmt.Sprintf("; %d of the %d open ones addressed", addressed, len(p.Addressed))
		}
	case engine.VerifyEnded:
		tell = decoded[engine.VerifyEndedPayload](e.Payload).Result
	case engine.PrepareFailed:
		tell = decoded[engine.PrepareFailedPayload](e.Payload).Step
	case engine.Stopped:
		p := decoded[engine.StoppedPayload](e.Payload)
		tell = p.Reason + ": " + p.Explanation
	case engine.RunStarted:
		tell = runText(decoded[engine.RunStartedPayload](e.Payload))
	case engine.SessionCut:
		tell = "cut after " + counted(decoded[engine.SessionCutPayload](e.Payload).Turns, "turn")
	case engine.PublishStarted, engine.ReadyStarted, engine.ReplyStarted:
		tell = decoded[struct{ Why string }](e.Payload).Why
	case engine.Published:
		p := decoded[engine.PublishedPayload](e.Payload)
		tell = fmt.Sprintf("pull request %s on branch %s", p.URL, p.Branch)
	case engine.PRObserved:
		tell = observedText(decoded[engine.Observation](e.Payload))
	case engine.PRReady:
		tell = "the pull request is marked ready for review"
	case engine.Replied:
		tell = "the reviewers have CAVU's answer in a comment on the pull request"
	case engine.HomeRemoved:
		tell = "the task's home is removed, since the task can run no more"
	default:
		tell = string(e.Payload)
	}

	l := strings.Join(strings.Fields(e.At.Format(time.TimeOnly)+" "+e.Type+" "+tell), " ")

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

// runText says why a run started: "retry" is a fresh run of a task that ran before.
func runText(p engine.RunStartedPayload) string {
	switch p.Reason {
	case "initial":
		return "the task's first run"
	case "retry":
		return "a fresh run of the task, which ran before"
	case engine.ReasonCIFix:
		return "a fix run on the pull request's failed checks, from " + p.Head
	case engine.ReasonComments:
		return fmt.Sprintf("a follow-up run on the reviewers' requests %s, from %s", strings.Join(p.Requests, ", "), p.Head)
	}

	return p.Reason
}

// observedText is what a look showed of the pull request. A reviewer's
// request is counted and never quoted.
func observedText(o engine.Observation) string {
	s := o.State
	if o.Draft {
		s += ", a draft"
	}

	if o.Conflict {
		s += ", in conflict with its base branch"
	}

	return fmt.Sprintf("%s; checks %s; %s of reviewers", s, checksText(o.Checks, o.Failed), counted(len(o.Comments), "open request"))
}

// decoded is the payload as T, with zero values where it does not fit.
func decoded[T any](payload []byte) T {
	var v T

	_ = json.Unmarshal(payload, &v)

	return v
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
