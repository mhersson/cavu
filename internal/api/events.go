package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

const (
	pageSize  = 200  // the events in a page whose query names no limit
	pageMax   = 1000 // the most events in a page, and in one read of a stream
	keepAlive = 15 * time.Second
)

// eventJSON is an event as the API shows it.
type eventJSON struct {
	ID      int64           `json:"id"`
	TaskID  int64           `json:"task_id"`
	RunID   *int64          `json:"run_id"`
	At      time.Time       `json:"at"`
	Type    string          `json:"type"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload"`
	Text    string          `json:"text"`
}

func eventView(e store.Event) eventJSON {
	v := eventJSON{ID: e.ID, TaskID: e.TaskID, At: e.At.UTC(), Type: e.Type, Actor: e.Actor, Payload: e.Payload, Text: engine.EventText(e)}
	if e.RunID != 0 {
		v.RunID = new(e.RunID)
	}

	return v
}

// number reads a whole number from a query value or a header; "" is 0.
func number(name, v string) (int64, error) {
	if v == "" {
		return 0, nil
	}

	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, &apiError{http.StatusBadRequest, name + " is not a whole number"}
	}

	return n, nil
}

// boolean reads true or false from a query value; "" is absent.
func boolean(name, v string, absent bool) (bool, error) {
	if v == "" {
		return absent, nil
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, &apiError{http.StatusBadRequest, fmt.Sprintf("%s %q: want true or false", name, v)}
	}

	return b, nil
}

// newestFirst reads a query's order: oldest, which "" is too, or newest.
func newestFirst(order string) (bool, error) {
	switch order {
	case "", "oldest":
		return false, nil
	case "newest":
		return true, nil
	}

	return false, &apiError{http.StatusBadRequest, fmt.Sprintf("order %q: want oldest or newest", order)}
}

// events answers a page of the task's events after the query's after, oldest
// first, or with order=newest below it, newest first; agent=false leaves out
// the agent's.
func (s *server) events(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	q := r.URL.Query()

	after, err := number("after", q.Get("after"))
	if err != nil {
		return err
	}

	limit, err := number("limit", q.Get("limit"))
	if err != nil {
		return err
	}

	newest, err := newestFirst(q.Get("order"))
	if err != nil {
		return err
	}

	agent, err := boolean("agent", q.Get("agent"), true)
	if err != nil {
		return err
	}

	if _, err := s.Store.Task(r.Context(), id); err != nil {
		return fmt.Errorf("task %d: %w", id, err)
	}

	p := store.EventPage{After: after, Limit: int(cmp.Or(min(limit, pageMax), pageSize)), Newest: newest}
	if !agent {
		p.OmitActor = "agent"
	}

	events, err := s.Store.EventsPage(r.Context(), id, p)
	if err != nil {
		return err
	}

	out := make([]eventJSON, len(events))
	for i, e := range events {
		out[i] = eventView(e)
	}

	reply(w, http.StatusOK, map[string][]eventJSON{"events": out})

	return nil
}

// stream sends the events of every task, or of the task the query names, as
// they are stored, until the client leaves or the server stops. Other
// processes store events too, so it polls the store rather than hear of them.
// While it is open, it holds the focus of the push device the query names.
func (s *server) stream(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	task, err := number("task", r.URL.Query().Get("task"))
	if err != nil {
		return err
	}

	device, err := number("device", r.URL.Query().Get("device"))
	if err != nil {
		return err
	}

	if task != 0 {
		if _, err := s.Store.Task(ctx, task); err != nil {
			return fmt.Errorf("task %d: %w", task, err)
		}
	}

	cursor, err := s.start(r)
	if err != nil {
		return err
	}

	if device != 0 {
		defer s.Focus.Connect(device)()
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // else a proxy such as nginx holds the events back
	w.WriteHeader(http.StatusOK)

	poll := time.NewTicker(cmp.Or(s.Poll, time.Second))
	defer poll.Stop()

	alive := time.NewTicker(keepAlive)
	defer alive.Stop()

	// The status line and the cursor go out at once, so a client that drops
	// before the first event resumes from the cursor with Last-Event-ID.
	fmt.Fprintf(w, "id: %d\n\n", cursor) //nolint:gosec // G705: the cursor is a parsed whole number, never the header's text

	rc := http.NewResponseController(w)
	err = rc.Flush()

	for err == nil {
		select {
		case <-ctx.Done():
			return nil
		case <-s.Stop:
			return nil
		case <-alive.C:
			_, err = io.WriteString(w, ":\n\n")
		case <-poll.C:
			cursor, err = s.send(ctx, w, cursor, task)
		}

		if err == nil {
			err = rc.Flush()
		}
	}

	// A failed write ends ctx, so an error while ctx lives is the server's.
	if ctx.Err() == nil {
		slog.ErrorContext(ctx, "request failed", "route", r.Pattern, "err", err)
	}

	return nil
}

// start is the id a stream starts after: Last-Event-ID, else the query's
// after, else the newest event's.
func (s *server) start(r *http.Request) (int64, error) {
	if from := cmp.Or(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")); from != "" {
		return number("Last-Event-ID or after", from)
	}

	newest, err := s.Store.LastEventIDs(r.Context())
	if err != nil {
		return 0, err
	}

	var id int64
	for _, last := range newest {
		id = max(id, last)
	}

	return id, nil
}

// send writes every event after cursor, of task when it is not 0, a page at a
// time, and returns the id of the last one written.
func (s *server) send(ctx context.Context, w io.Writer, cursor, task int64) (int64, error) {
	for {
		events, err := s.Store.EventsAfter(ctx, cursor, task, pageMax)
		if err != nil {
			return cursor, err
		}

		for _, e := range events {
			// Marshal compacts the payload too, so the data is one line.
			data, err := json.Marshal(eventView(e))
			if err != nil {
				return cursor, err
			}

			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, data)
			cursor = e.ID
		}

		if len(events) < pageMax {
			return cursor, nil
		}
	}
}
