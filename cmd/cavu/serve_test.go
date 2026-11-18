package main

import (
	"context"
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
