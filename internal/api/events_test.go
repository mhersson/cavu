package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// within bounds how long a test waits for a stream.
const within = 10 * time.Second

// say appends n events of the agent's text to the task in one write, with the
// run when runID is not 0, and returns them as stored.
func (f *fixture) say(t *testing.T, taskID, runID int64, n int) []store.Event {
	t.Helper()

	events := make([]store.Event, n)
	for i := range events {
		events[i] = store.Event{
			TaskID: taskID, RunID: runID, At: f.clock.Now(), Type: engine.AgentText, Actor: "agent",
			Payload: json.RawMessage(fmt.Sprintf(`{"text": "line %d"}`, i)),
		}
	}

	last, err := f.store.Append(t.Context(), taskID, -1, events, nil)
	require.NoError(t, err)

	// The events of one write have consecutive ids.
	stored, err := f.store.EventsAfter(t.Context(), last-int64(n), taskID, 0)
	require.NoError(t, err)
	require.Len(t, stored, n)

	return stored
}

// wire is the event as the API shows it.
func wire(e store.Event) string {
	run := "null"
	if e.RunID != 0 {
		run = strconv.FormatInt(e.RunID, 10)
	}

	return fmt.Sprintf(`{"id": %d, "task_id": %d, "run_id": %s, "at": %q, "type": %q, "actor": %q, "payload": %s, "text": %q}`,
		e.ID, e.TaskID, run, rfc3339(e.At), e.Type, e.Actor, e.Payload, engine.EventText(e))
}

// page is the body of a page of events.
func page(events []store.Event) string {
	items := make([]string, len(events))
	for i, e := range events {
		items[i] = wire(e)
	}

	return `{"events": [` + strings.Join(items, ", ") + `]}`
}

func TestEventCarriesItsSentence(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	f.say(t, id, 0, 1)

	status, body := f.call(t, fmt.Sprintf("GET /api/tasks/%d/events", id), "")
	require.Equal(t, http.StatusOK, status, body)

	var page struct{ Events []struct{ Text string } }
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	require.Len(t, page.Events, 1)
	assert.Contains(t, page.Events[0].Text, "line 0", "the agent's text is a fact of the sentence")
}

func TestEventPages(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	other := f.task(t, `{"project": "site", "title": "Add a header"}`)

	run, err := f.store.CreateRun(t.Context(), store.Run{TaskID: id, Reason: "initial"})
	require.NoError(t, err)

	// The other task's events lie among this task's, and no page holds them.
	events := f.say(t, id, run.ID, 300)
	f.say(t, other, 0, 10)
	events = append(events, f.say(t, id, 0, 150)...)

	get := func(t *testing.T, query string) string {
		t.Helper()

		status, body := f.call(t, fmt.Sprintf("GET /api/tasks/%d/events%s", id, query), "")
		require.Equal(t, http.StatusOK, status, body)

		return body
	}

	for _, tc := range []struct {
		query string
		want  []store.Event
	}{
		{"", events[:200]},
		{fmt.Sprintf("?after=%d", events[199].ID), events[200:400]},
		{fmt.Sprintf("?after=%d", events[399].ID), events[400:]},
		{fmt.Sprintf("?after=%d", events[449].ID), nil},
		{"?limit=5", events[:5]},
		{"?limit=0", events[:200]},
	} {
		t.Run("the page "+tc.query, func(t *testing.T) {
			assert.JSONEq(t, page(tc.want), get(t, tc.query))
		})
	}

	t.Run("a limit above 1000", func(t *testing.T) {
		events = append(events, f.say(t, id, 0, 700)...)
		assert.JSONEq(t, page(events[:1000]), get(t, "?limit=5000"))
	})
}

// mixed gives the task twelve events of the engine's, each followed by three
// of the agent's, and returns every event of the task as stored.
func (f *fixture) mixed(t *testing.T, taskID int64) []store.Event {
	t.Helper()

	for range 12 {
		f.add(t, taskID, engine.VerifyEnded, engine.VerifyEndedPayload{Result: engine.ResultPassed})
		f.say(t, taskID, 0, 3)
	}

	all, err := f.store.Events(t.Context(), taskID, 0, 0)
	require.NoError(t, err)

	return all
}

func TestEventsNewestFirstWithoutTheAgent(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)

	all := f.mixed(t, id)

	var want []store.Event // the events that are not the agent's, newest first

	for _, e := range slices.Backward(all) {
		if e.Actor != "agent" {
			want = append(want, e)
		}
	}

	require.Len(t, want, 12)

	for _, tc := range []struct {
		after string
		want  []store.Event
	}{
		{"", want[:5]},
		{fmt.Sprintf("&after=%d", want[4].ID), want[5:10]},
		{fmt.Sprintf("&after=%d", want[9].ID), want[10:]},
		{fmt.Sprintf("&after=%d", want[11].ID), nil},
	} {
		status, body := f.call(t, fmt.Sprintf("GET /api/tasks/%d/events?order=newest&agent=false&limit=5%s", id, tc.after), "")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, page(tc.want), body, "the page after %q", tc.after)
	}
}

func TestEventsDefaultIsUnchanged(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	all := f.mixed(t, id)

	for _, query := range []string{"", "?order=oldest&agent=true"} {
		status, body := f.call(t, fmt.Sprintf("GET /api/tasks/%d/events%s", id, query), "")
		require.Equal(t, http.StatusOK, status, body)
		assert.JSONEq(t, page(all), body, "every event, the agent's too, oldest first, for %q", query)
	}
}

func TestEventRequestsRefused(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	f.say(t, id, 0, 3)

	for _, tc := range []struct {
		name, route, lastEventID string
		want                     int
	}{
		{"a page of an unknown task", "GET /api/tasks/99/events", "", http.StatusNotFound},
		{"a page whose limit is not a number", fmt.Sprintf("GET /api/tasks/%d/events?limit=many", id), "", http.StatusBadRequest},
		{"a page after a negative id", fmt.Sprintf("GET /api/tasks/%d/events?after=-1", id), "", http.StatusBadRequest},
		{"a page in no order", fmt.Sprintf("GET /api/tasks/%d/events?order=sideways", id), "", http.StatusBadRequest},
		{"a page whose agent is not a boolean", fmt.Sprintf("GET /api/tasks/%d/events?agent=maybe", id), "", http.StatusBadRequest},
		{"a stream of an unknown task", "GET /api/events?task=99", "", http.StatusNotFound},
		{"a stream after what is not a number", "GET /api/events?after=last", "", http.StatusBadRequest},
		{"a stream whose device is not a number", "GET /api/events?device=phone", "", http.StatusBadRequest},
		{"a stream whose Last-Event-ID is not a number", "GET /api/events", "last", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request(t, tc.route, "")
			r.Header.Set("Authorization", "Bearer "+f.token)

			if tc.lastEventID != "" {
				r.Header.Set("Last-Event-ID", tc.lastEventID)
			}

			resp, body := send(t, r)
			require.Equal(t, tc.want, resp.StatusCode, body)
			assertError(t, body)
		})
	}
}

// stream is an open event stream. Its messages after the opening one arrive
// on msgs, which is closed when the stream ends.
type stream struct {
	resp   *http.Response
	msgs   chan message
	opened string // the id the opening message holds
}

// message is one message of a stream: its id line and its data line.
type message struct{ id, data string }

// connect sends r over a connection of its own and returns the response as soon
// as its headers arrive.
func connect(t *testing.T, r *http.Request) *http.Response {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: within}}

	resp, err := client.Do(r)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

// stream opens the event stream with the query under the API token, sending
// Last-Event-ID when lastEventID is not "".
func (f *fixture) stream(t *testing.T, query, lastEventID string) *stream {
	t.Helper()

	r := f.request(t, "GET /api/events"+query, "")
	r.Header.Set("Authorization", "Bearer "+f.token)

	if lastEventID != "" {
		r.Header.Set("Last-Event-ID", lastEventID)
	}

	resp := connect(t, r)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	s := &stream{resp: resp, msgs: make(chan message)}
	go s.read()

	// Closing the body ends read, which may be waiting to send a message.
	t.Cleanup(func() {
		_ = resp.Body.Close()

		for range s.msgs {
		}
	})

	select {
	case m, ok := <-s.msgs:
		require.True(t, ok, "the stream ended before it opened")
		require.Empty(t, m.data, "the opening message holds only an id")
		s.opened = m.id
	case <-time.After(within):
		require.FailNow(t, "the stream sent no opening message")
	}

	return s
}

// read sends each message of the body on msgs, skipping comment lines.
func (s *stream) read() {
	defer close(s.msgs)

	var m message

	sc := bufio.NewScanner(s.resp.Body)
	for sc.Scan() {
		line := sc.Text()

		if v, ok := strings.CutPrefix(line, "id: "); ok {
			m.id = v
		}

		if v, ok := strings.CutPrefix(line, "data: "); ok {
			m.data = v
		}

		if line == "" && m != (message{}) {
			s.msgs <- m

			m = message{}
		}
	}
}

// expect wants the stream's next message to be e under its id.
func (s *stream) expect(t *testing.T, e store.Event) {
	t.Helper()

	select {
	case m, ok := <-s.msgs:
		require.True(t, ok, "the stream ended before event %d", e.ID)
		require.Equal(t, strconv.FormatInt(e.ID, 10), m.id)
		assert.JSONEq(t, wire(e), m.data)
	case <-time.After(within):
		require.FailNow(t, "no message arrived", "want event %d", e.ID)
	}
}

func TestStreamSendsNewEvents(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	a := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	b := f.task(t, `{"project": "site", "title": "Add a header"}`)

	// Older than the streams, with b's newest event the newest of all.
	f.say(t, a, 0, 3)
	newest := f.say(t, b, 0, 2)[1]

	every := f.stream(t, "", "")
	ofA := f.stream(t, fmt.Sprintf("?task=%d", a), "")

	assert.Equal(t, strconv.FormatInt(newest.ID, 10), every.opened, "the stream opens on the id it starts after")

	assert.Equal(t, "text/event-stream", every.resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", every.resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", every.resp.Header.Get("X-Accel-Buffering"))

	first, second, later := f.say(t, a, 0, 1)[0], f.say(t, b, 0, 1)[0], f.say(t, a, 0, 2)

	// The first message is the first new event, so nothing older arrives; a's
	// stream ends on a's newest, so b's, which lies before it, never comes.
	for _, e := range append([]store.Event{first, second}, later...) {
		every.expect(t, e)
	}

	for _, e := range append([]store.Event{first}, later...) {
		ofA.expect(t, e)
	}
}

func TestStreamResumes(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	id := f.task(t, `{"project": "site", "title": "Add a footer"}`)
	sent := f.say(t, id, 0, 4)
	from := strconv.FormatInt(sent[1].ID, 10)

	for _, tc := range []struct {
		name, query, lastEventID string
	}{
		{"after Last-Event-ID", "", from},
		{"after the query's after", "?after=" + from, ""},
		{"after Last-Event-ID, not the query's after", fmt.Sprintf("?after=%d", sent[3].ID), from},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := f.stream(t, tc.query, tc.lastEventID)
			assert.Equal(t, from, s.opened)

			missed, err := f.store.EventsAfter(t.Context(), sent[1].ID, id, 0)
			require.NoError(t, err)

			for _, e := range append(missed, f.say(t, id, 0, 2)...) {
				s.expect(t, e)
			}

			// The newest event comes next, so none came twice.
			s.expect(t, f.say(t, id, 0, 1)[0])
		})
	}
}

func TestStreamNeedsAuthority(t *testing.T) {
	f := newServer(t)

	resp, body := send(t, f.request(t, "GET /api/events", ""))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	assertError(t, body)

	// A GET needs no CSRF header, so a browser's EventSource can open it.
	resp = connect(t, withCookie(f.request(t, "GET /api/events", ""), f.login(t).Value, false))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
}

func TestStreamEndsWithTheServer(t *testing.T) {
	f := newServer(t)
	s := f.stream(t, "", "")

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	require.NoError(t, f.srv.Config.Shutdown(ctx), "the open stream does not hold Shutdown until its deadline")

	select {
	case _, ok := <-s.msgs:
		assert.False(t, ok, "the stream ends")
	case <-time.After(within):
		require.FailNow(t, "the stream did not end")
	}

	f.srv.Close() // returns, so no handler is left running
}
