package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// WaitingForSlot is why a task that would act does not, while every slot is taken.
const WaitingForSlot = "Waiting for a free slot."

// The poller's clock: the next look at a watched task's pull request comes
// fastLook after a look that answered fast, else slowLook; after a failed look
// firstBackoff, doubled for each failure in a row up to maxBackoff; under a
// rate limit at the later of that and when it lifts, at most maxRateWait away.
const (
	fastLook     = 10 * time.Second
	slowLook     = time.Minute
	firstBackoff = 10 * time.Second
	maxBackoff   = 5 * time.Minute
	maxRateWait  = time.Hour
)

// Scheduler drives every task that can act, at most the concurrency setting at
// a time, and runs requested environment checks one at a time, each in a slot.
// It looks at the pull request of each task Decide watches on a clock of its
// own, which decides nothing and is kept in memory only. What else it keeps is
// a cache of the store, so a new Scheduler carries on where an old one stopped.
type Scheduler struct {
	Loop    *Loop
	Tick    time.Duration                 // 0 means 2 seconds
	Only    func(store.Task) bool         // nil: every task
	Checks  bool                          // also run requested environment checks
	OnError func(taskID int64, err error) // an error met, not the end of ctx; taskID 0 when no task's; nil drops it

	once    sync.Once
	nudge   chan struct{}
	passed  func()       // a test's hook, called at the end of each pass
	driving atomic.Int64 // drives in flight

	mu      sync.Mutex
	waiting map[int64]string   // why a task does not act
	stops   map[int64]StopFact // a task's open stop, as the newest pass to read the task saw it
}

// round is what one Run keeps. Only Run's goroutine touches it.
type round struct {
	drives   map[int64]*flight
	polls    map[int64]*poll // each watched task's clock, kept until it rests: a stop between two looks keeps a rate limit's wait
	seen     map[int64]int64 // a task's newest event id when it was found with nothing to drive
	loose    map[int64]bool  // tasks whose last drive may have left containers up
	checking bool
	ends     chan end
	looks    chan look
	checked  chan error
}

// flight is a drive in progress.
type flight struct {
	cancel  context.CancelFunc
	checked int64 // the newest event id read for a person's cancel
	slot    bool  // the drive's action starts a container
}

// poll is the poller's clock of a watched task.
type poll struct {
	until   time.Time     // the watch's Until, when a clock rule of Decide's may end it
	next    time.Time     // when the next look is due
	backoff time.Duration // the wait after the newest look's failure, 0 after a success
	failed  error         // the newest look's error, nil after a success
	looking bool          // a look is in flight
}

// due is when the watched task is read again without an event.
func (p *poll) due() time.Time {
	if !p.until.IsZero() && p.until.Before(p.next) {
		return p.until
	}

	return p.next
}

// look is how a look at a task's pull request ended.
type look struct {
	id   int64
	at   time.Time // when the pass started it
	fast bool
	err  error
}

// end is how a drive ended.
type end struct {
	id, last int64 // last: the task's newest event id when the drive failed
	err      error
	cut      bool // the drive's context ended first
}

// Run decides and drives until ctx ends. It then interrupts what is in flight,
// releases each open run it drove, keeping its volumes, and returns nil.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(cmp.Or(s.Tick, 2*time.Second))
	defer ticker.Stop()

	s.mu.Lock()
	s.waiting, s.stops = map[int64]string{}, map[int64]StopFact{}
	s.mu.Unlock()

	s.Loop.Paused = s.paused
	r := &round{
		drives: map[int64]*flight{}, polls: map[int64]*poll{}, seen: map[int64]int64{}, loose: map[int64]bool{},
		ends: make(chan end), looks: make(chan look), checked: make(chan error, 1),
	}
	nudge := s.nudges()

	for ctx.Err() == nil {
		s.pass(ctx, r)

		select {
		case <-ctx.Done():
		case <-ticker.C:
		case <-nudge:
		case e := <-r.ends:
			s.ended(ctx, r, e)
		case l := <-r.looks:
			s.looked(ctx, r, l)
		case err := <-r.checked:
			r.checking = false

			s.report(ctx, 0, err)
		}
	}

	for len(r.drives) > 0 {
		s.ended(ctx, r, <-r.ends)
	}

	for _, p := range r.polls {
		if p.looking {
			<-r.looks
		}
	}

	if r.checking {
		s.report(ctx, 0, <-r.checked)
	}

	for _, id := range slices.Sorted(maps.Keys(r.loose)) {
		rctx, cancel := recording(ctx)
		s.report(rctx, id, s.Loop.ReleaseOpenRun(rctx, id))
		cancel()
	}

	return nil
}

// Nudge makes Run decide now instead of at its next tick.
func (s *Scheduler) Nudge() {
	select {
	case s.nudges() <- struct{}{}:
	default:
	}
}

func (s *Scheduler) nudges() chan struct{} {
	s.once.Do(func() { s.nudge = make(chan struct{}, 1) })

	return s.nudge
}

// Driving reports whether a drive is in flight. A task can rest before its
// drive ends, while the drive still releases the task's run.
func (s *Scheduler) Driving() bool { return s.driving.Load() > 0 }

// Waiting is why the task is not acting: a wait's or a watch's reason, the
// error of the newest look at a watched task's pull request and when the next
// comes, WaitingForSlot, or "".
func (s *Scheduler) Waiting(taskID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.waiting[taskID]
}

// paused answers Loop.Paused from the open stops the passes saw.
func (s *Scheduler) paused(taskID int64) Pause {
	s.mu.Lock()
	defer s.mu.Unlock()

	return pauseOf(s.stops, taskID)
}

func (s *Scheduler) pass(ctx context.Context, r *round) {
	if s.passed != nil {
		defer s.passed()
	}

	set, err := s.Loop.Store.Settings(ctx)
	if err != nil {
		s.report(ctx, 0, err)

		return
	}

	slots, err := Concurrency(set)
	if err != nil {
		s.report(ctx, 0, err)

		return
	}

	ids, err := s.Loop.Store.LastEventIDs(ctx)
	if err != nil {
		s.report(ctx, 0, err)

		return
	}

	now := s.Loop.Now()

	// Every task is read before any drive starts, so each decision, and each
	// drive's first, sees every open stop. A task Only leaves out still pauses the rest.
	var acts []Snapshot

	for _, id := range slices.Sorted(maps.Keys(ids)) {
		if f := r.drives[id]; f != nil {
			s.cancelIfAsked(ctx, id, f, ids[id])

			continue
		}

		// A watched task is also read when its clock comes due, and never while a look is in flight.
		if p := r.polls[id]; p != nil && p.looking || r.seen[id] == ids[id] && (p == nil || now.Before(p.due())) {
			continue
		}

		snap, err := s.Loop.Snapshot(ctx, id)
		if err != nil {
			s.report(ctx, id, err)
			r.seen[id] = ids[id]
			delete(r.polls, id) // read again only after an event, so the error is reported once

			continue
		}

		s.mu.Lock()
		if st := snap.Facts.OpenStop; st != nil {
			s.stops[id] = *st
		} else {
			delete(s.stops, id)
		}
		s.mu.Unlock()

		if s.Only != nil && !s.Only(snap.Task) {
			r.seen[id] = snap.LastEventID

			continue
		}

		acts = append(acts, snap)
	}

	// A requested check takes the first free slot, before any task.
	if s.Checks && !r.checking && r.busy() < slots {
		s.check(ctx, r)
	}

	for _, snap := range acts {
		id := snap.Task.ID
		snap.Pause = s.paused(id) // now from every stop this pass read
		a := Decide(snap)
		why := ""

		switch {
		case a.Kind == KindWatch:
			why = s.watch(ctx, r, id, a, now)
			r.seen[id] = snap.LastEventID
		case a.Kind == KindNone:
			r.seen[id] = snap.LastEventID
			delete(r.polls, id)
		case a.Kind == KindWait:
			why = a.Why
		case slotless(a.Kind) || r.busy() < slots:
			s.drive(ctx, r, snap, !slotless(a.Kind))
		default:
			why = WaitingForSlot
		}

		s.mu.Lock()
		if why != "" {
			s.waiting[id] = why
		} else {
			delete(s.waiting, id)
		}
		s.mu.Unlock()
	}
}

// slotless reports whether an action starts no container, so its drive takes no slot.
func slotless(k ActionKind) bool { return k == KindStop || k == KindReady || k == KindClean }

// busy counts the slots taken: one per drive that starts a container, and one
// for a running check.
func (r *round) busy() int {
	n := 0

	for _, f := range r.drives {
		if f.slot {
			n++
		}
	}

	if r.checking {
		n++
	}

	return n
}

// watch looks at the task's pull request when its next look is due, off the
// pass's goroutine and in no slot, and returns why the task waits.
func (s *Scheduler) watch(ctx context.Context, r *round, id int64, a Action, now time.Time) string {
	p := r.polls[id]
	if p == nil {
		p = &poll{} // due at once: a new Scheduler looks in its first pass
		r.polls[id] = p
	}

	p.until = a.Until

	if !now.Before(p.next) {
		p.looking = true
		looks := r.looks

		go func() {
			fast, err := s.Loop.Observe(ctx, id)
			looks <- look{id: id, at: now, fast: fast, err: err}
		}()
	}

	if p.failed != nil {
		return fmt.Sprintf("The last look at the pull request failed: %v. The next look is at %s.", p.failed, p.next.UTC().Format(time.RFC3339))
	}

	return a.Why
}

// looked sets the task's next look by how its last one ended, and has the next
// pass read the task again. A run of failures is reported once; Waiting tells the newest.
func (s *Scheduler) looked(ctx context.Context, r *round, l look) {
	p := r.polls[l.id]
	p.looking = false

	delete(r.seen, l.id)

	if l.err == nil {
		p.backoff, p.failed = 0, nil
		p.next = l.at.Add(slowLook)

		if l.fast {
			p.next = l.at.Add(fastLook)
		}

		return
	}

	if p.failed == nil {
		s.report(ctx, l.id, l.err)
	}

	p.failed = l.err
	p.backoff = min(max(2*p.backoff, firstBackoff), maxBackoff)
	wait := p.backoff

	// A reset that is not ahead, as after a Retry-After of 0, still waits the backoff.
	if limited, ok := errors.AsType[*RateLimited](l.err); ok {
		wait = min(max(wait, limited.Until.Sub(l.at)), maxRateWait)
	}

	p.next = l.at.Add(wait)
}

// cancelIfAsked ends the drive's context once a person's cancel of its task
// is recorded, reading only the events since its last look.
func (s *Scheduler) cancelIfAsked(ctx context.Context, id int64, f *flight, last int64) {
	if last == f.checked {
		return
	}

	evs, err := s.Loop.Store.Events(ctx, id, f.checked, 0)
	if err != nil {
		s.report(ctx, id, err)

		return
	}

	for _, e := range evs {
		if e.Type == HumanCancelled {
			f.cancel()
		}

		f.checked = e.ID
	}
}

func (s *Scheduler) drive(ctx context.Context, r *round, snap Snapshot, slot bool) {
	id := snap.Task.ID
	dctx, cancel := context.WithCancel(ctx)
	r.drives[id] = &flight{cancel: cancel, checked: snap.LastEventID, slot: slot}
	r.loose[id] = true
	ends := r.ends

	s.driving.Add(1)

	go func() {
		defer cancel()

		_, err := s.Loop.RunTask(dctx, id)
		e := end{id: id, err: err, cut: dctx.Err() != nil}

		if err != nil && !e.cut {
			var lerr error

			e.last, lerr = s.Loop.Store.LastEventID(dctx, id)
			e.err = errors.Join(err, lerr)
		}

		ends <- e
	}()
}

func (s *Scheduler) ended(ctx context.Context, r *round, e end) {
	delete(r.drives, e.id)
	s.driving.Add(-1)

	switch {
	case e.cut: // the next pass, or the end of Run, takes the task up
	case e.err != nil:
		s.report(ctx, e.id, e.err)
		r.seen[e.id] = e.last
		delete(r.polls, e.id) // read again only after an event, or a failing clean would repeat at every pass
	default:
		delete(r.loose, e.id) // a drive that rests or waits has released its run
	}
}

// check runs the oldest requested environment check in a slot.
func (s *Scheduler) check(ctx context.Context, r *round) {
	ps, err := s.Loop.Store.Projects(ctx)
	if err != nil {
		s.report(ctx, 0, err)

		return
	}

	var next *store.Project

	for i, p := range ps {
		if p.CheckRequestedAt != nil && (next == nil || p.CheckRequestedAt.Before(*next.CheckRequestedAt)) {
			next = &ps[i]
		}
	}

	if next == nil {
		return
	}

	r.checking = true
	checked := r.checked

	// CheckProject's two halves, so only a failure to store is reported: the
	// check's own error is the fault it stores.
	go func() {
		started, res, suggestion, err := s.Loop.checkEnvironment(ctx, next.ID)
		err = s.Loop.storeCheck(ctx, started, *next.CheckRequestedAt, res, suggestion, err)

		if errors.Is(err, errChangedDuringCheck) {
			err = nil // the request stays, so the next pass checks again
		}

		checked <- err
	}()
}

// report passes err to OnError unless err is nil or ctx has ended.
func (s *Scheduler) report(ctx context.Context, taskID int64, err error) {
	if err != nil && ctx.Err() == nil && s.OnError != nil {
		s.OnError(taskID, err)
	}
}
