package push

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// titles are a notification's words for each way a task needs a person.
var titles = map[string]string{
	engine.ClearAnswer: "Needs an answer",
	engine.NeedsReview: "Ready for review",
	engine.ClearRetry:  "Stopped",
	engine.NeedsTake:   "A change to take",
}

// Notifier pushes each way a task comes to need a person to every subscribed
// device but the ones being looked at, once per task, way and since-time. It
// reads the view and decides nothing.
type Notifier struct {
	List    func(context.Context) ([]engine.Row, error) // the tasks that need a person
	Store   *store.Store
	Sender  *Sender
	Focus   *Focus
	Tick    time.Duration // 0 means 2 seconds
	OnError func(error)   // an error met, not the end of ctx; nil drops it
}

// Run notifies at once and then every Tick until ctx ends.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(cmp.Or(n.Tick, 2*time.Second))
	defer ticker.Stop()

	for ctx.Err() == nil {
		n.pass(ctx)

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	return nil
}

// pass notifies every need not notified yet. With no subscription it only
// records them, so a first subscription does not replay old needs.
func (n *Notifier) pass(ctx context.Context) {
	rows, err := n.List(ctx)
	if err != nil {
		n.report(ctx, err)

		return
	}

	notified, err := n.Store.PushNotified(ctx)
	if err != nil {
		n.report(ctx, err)

		return
	}

	subs, err := n.Store.PushSubscriptions(ctx)
	if err != nil {
		n.report(ctx, err)

		return
	}

	for _, r := range rows {
		now := store.Notified{Way: r.Needs, Since: *r.NeedsYouSince}
		// The table keeps milliseconds.
		if was, ok := notified[r.ID]; ok && was.Way == now.Way && was.Since.UnixMilli() == now.Since.UnixMilli() {
			continue
		}

		subs = n.send(ctx, r, subs)

		if err := n.Store.MarkPushNotified(ctx, r.ID, now); err != nil {
			n.report(ctx, fmt.Errorf("task %d: %w", r.ID, err))
		}
	}
}

// send pushes r's need to every subscription in subs not being looked at, and
// returns subs without the ones their push service no longer knows. An error
// names a subscription by its id, since its endpoint is a capability.
func (n *Notifier) send(ctx context.Context, r engine.Row, subs []store.PushSubscription) []store.PushSubscription {
	id := strconv.FormatInt(r.ID, 10)
	m := Message{Title: titles[r.Needs] + " · " + r.Project, Body: r.Title, URL: "/tasks/" + id, Tag: "task-" + id}
	kept := subs[:0]

	for _, sub := range subs {
		if n.Focus.Looking(sub.ID) {
			kept = append(kept, sub)

			continue
		}

		switch err := n.Sender.Send(ctx, sub, m); {
		case errors.Is(err, ErrGone):
			if err := n.Store.RemovePushSubscription(ctx, sub.ID); err != nil {
				n.report(ctx, fmt.Errorf("drop push subscription %d: %w", sub.ID, err))
			}

			continue
		case err != nil:
			n.report(ctx, fmt.Errorf("task %d: push subscription %d: %w", r.ID, sub.ID, err))
		default:
			if err := n.Store.PushDelivered(ctx, sub.ID, time.Now()); err != nil {
				n.report(ctx, fmt.Errorf("push subscription %d: %w", sub.ID, err))
			}
		}

		kept = append(kept, sub)
	}

	return kept
}

// report passes err to OnError unless ctx has ended.
func (n *Notifier) report(ctx context.Context, err error) {
	if ctx.Err() == nil && n.OnError != nil {
		n.OnError(err)
	}
}
