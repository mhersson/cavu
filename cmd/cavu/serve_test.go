package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveURL is the URL the verb's output says it listens on, or "".
func serveURL(out string) string {
	for f := range strings.FieldsSeq(out) {
		if strings.HasPrefix(f, "http://") {
			return f
		}
	}

	return ""
}

func TestServeStartsDrivesAndStops(t *testing.T) {
	dir, id, fake := runSetup(t)
	cavuIn(t, dir, "settings", "set", "listen_address", "127.0.0.1:0")

	a, err := openApp(t.Context(), dir)
	require.NoError(t, err)
	require.NoError(t, a.actions().Start(t.Context(), taskID(t, id)))
	a.close()

	token := strings.TrimSpace(cavuIn(t, dir, "token", "add", "test"))

	// The session runs until the scheduler cuts it, so the run is in flight when the server stops.
	working := make(chan struct{})
	fake.runner.duringWork = func(ctx context.Context) {
		close(working)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, errOut, done := &syncBuffer{}, &syncBuffer{}, make(chan error, 1)

	go func() { done <- runServe(ctx, []string{"--data-dir", dir}, nil, out, errOut) }()

	require.Eventually(t, func() bool { return serveURL(out.String()) != "" }, within, time.Millisecond)

	url := serveURL(out.String())
	assert.True(t, strings.HasPrefix(url, "http://127.0.0.1:"), url)
	assert.NotEqual(t, "http://127.0.0.1:0", url, "the address bound, not the one asked for")

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/health", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/api/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	stream, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer stream.Body.Close()

	require.Equal(t, http.StatusOK, stream.StatusCode)

	select {
	case <-working:
	case <-time.After(within):
		require.FailNow(t, "the server did not drive the started task")
	}

	assert.Contains(t, errOut.String(), "cavu user set", "no user exists yet, and the verb that sets one is named")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(within):
		require.FailNow(t, "the verb did not end")
	}

	// A stream the server's stop ended closes cleanly; one cut once the grace ran out reads unexpected EOF.
	_, err = io.ReadAll(stream.Body)
	require.NoError(t, err, "the open stream ended when the server stopped")

	assert.Equal(t, []engine.ActionKind{engine.KindPrepare, engine.KindWork}, fake.runner.actions)

	task := storedTask(t, dir, id)

	a, err = openApp(t.Context(), dir)
	require.NoError(t, err)

	defer a.close()

	run, ok, err := a.store.OpenRun(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, ok, "the run stays open")

	assert.Equal(t, []release{{
		ref: engine.RunRef{
			InstanceID: storedSettings(t, dir)["instance_id"], Image: sandbox.ImageName(version),
			TaskID: task.ID, RunID: run.ID, ProjectID: task.ProjectID,
		},
		remove: engine.KeepVolumes,
	}}, fake.runner.releases, "the run in flight is released with its volumes kept")

	require.NoError(t, a.lock(roleVerb), "the lock is free")
}

func TestServeGivesTheEndOfAnotherTasksUsageLimit(t *testing.T) {
	dir, limited, _ := runSetup(t)
	reset := time.Now().Add(time.Hour).Truncate(time.Second).UTC()

	seedEvents(t, dir, limited, nil, event(engine.TaskStarted, "user", struct{}{}), event(engine.Stopped, "engine",
		engine.StoppedPayload{Reason: engine.StopUsageLimit, Explanation: "limit", Action: engine.ClearNone, ResetsAt: reset}))

	held := addTask(t, dir, "Add a footer")
	seedEvents(t, dir, held, nil, event(engine.TaskStarted, "user", struct{}{}))

	cavuIn(t, dir, "settings", "set", "listen_address", "127.0.0.1:0")
	token := strings.TrimSpace(cavuIn(t, dir, "token", "add", "test"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, done := &syncBuffer{}, make(chan error, 1)

	go func() { done <- runServe(ctx, []string{"--data-dir", dir}, nil, out, &syncBuffer{}) }()

	require.Eventually(t, func() bool { return serveURL(out.String()) != "" }, within, time.Millisecond)

	var got struct {
		Waiting      string
		WaitingUntil *string `json:"waiting_until"`
	}

	// The scheduler's first pass reads the other task's stop.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serveURL(out.String())+"/api/tasks/"+held, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		defer resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))

		return got.Waiting != ""
	}, within, 10*time.Millisecond)

	assert.Equal(t, new(reset.Format(time.RFC3339)), got.WaitingUntil, "the held task waits until the other task's limit resets")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(within):
		require.FailNow(t, "the verb did not end")
	}
}

func TestServeRunsTheNotifier(t *testing.T) {
	dir, id, _ := runSetup(t)
	seedEvents(t, dir, id, nil, event(engine.TaskStarted, "user", struct{}{}), question())
	cavuIn(t, dir, "settings", "set", "listen_address", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- runServe(ctx, []string{"--data-dir", dir}, nil, &syncBuffer{}, &syncBuffer{}) }()

	task := taskID(t, id)

	// With no subscription, the notifier records the need without sending it.
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		a, err := openApp(t.Context(), dir)
		require.NoError(c, err)

		defer a.close()

		notified, err := a.store.PushNotified(t.Context())
		require.NoError(c, err)
		assert.Equal(c, engine.ClearAnswer, notified[task].Way)
	}, within, 10*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(within):
		require.FailNow(t, "the verb did not end")
	}
}

func TestServeRefusedWhenLocked(t *testing.T) {
	for _, role := range []string{roleServe, roleVerb} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			holdLock(t, dir, role)
			refuseRuntime(t)

			code, stdout, stderr := runCavu("serve", "--data-dir", dir)

			assert.Equal(t, 1, code)
			assert.Empty(t, stdout, "nothing listens")
			// The directory holds the test's name, and so the role, too.
			assert.Contains(t, strings.ReplaceAll(stderr, dir, ""), role, "the holder's role is named")

			lock := filepath.Join(dir, "lock")
			if role == roleServe {
				assert.Contains(t, stderr, dir)
				assert.NotContains(t, stderr, lock, "the server is named, not the lock")
			} else {
				assert.Contains(t, stderr, lock)
			}
		})
	}
}

func TestServeServesThePage(t *testing.T) {
	dir, _, _ := runSetup(t)
	cavuIn(t, dir, "settings", "set", "listen_address", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	out, done := &syncBuffer{}, make(chan error, 1)

	go func() { done <- runServe(ctx, []string{"--data-dir", dir}, nil, out, &syncBuffer{}) }()

	require.Eventually(t, func() bool { return serveURL(out.String()) != "" }, within, time.Millisecond)

	get := func(path string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serveURL(out.String())+path, nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })

		return resp
	}

	page := get("/")
	assert.Equal(t, http.StatusOK, page.StatusCode)
	assert.Contains(t, page.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, page.Header.Get("Content-Security-Policy"), "default-src 'self'")

	health := get("/api/health")
	assert.Equal(t, http.StatusOK, health.StatusCode)
	assert.Contains(t, health.Header.Get("Content-Type"), "application/json")
	assert.Empty(t, health.Header.Get("Content-Security-Policy"))

	var body map[string]any
	require.NoError(t, json.NewDecoder(health.Body).Decode(&body))

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(within):
		require.FailNow(t, "the verb did not end")
	}
}
