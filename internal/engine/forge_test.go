package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withPublish turns the switch publish on for the harness's project.
func (h *harness) withPublish() {
	h.t.Helper()

	p := h.project()
	p.Switches["publish"] = true
	require.NoError(h.t, h.store.UpdateProject(h.t.Context(), p))
}

func TestPublishRecordsThePullRequest(t *testing.T) {
	h := newHarness(t)
	h.withPublish()

	res := workDone
	res.WhereToLook = []string{"login.go: the redirect after the form"}
	h.runner.works = []step[engine.WorkResult]{{res: res}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/remove"}, h.runner.trace())

	evs := h.events()
	require.GreaterOrEqual(t, len(evs), 4)

	tail := evs[len(evs)-4:]
	assert.Equal(t, []string{engine.Exported, engine.PublishStarted, engine.Published, engine.RunEnded}, types(tail))
	assert.Equal(t, "engine", tail[1].Actor)
	assert.NotEmpty(t, payloadOf[engine.PublishStartedPayload](t, tail[1]).Why)
	assert.Equal(t, "runner", tail[2].Actor)
	assert.Equal(t, engine.PublishedPayload{
		Branch: publishedOK.Branch, Head: publishedOK.Head, Number: publishedOK.Number, URL: publishedOK.URL, NodeID: publishedOK.NodeID,
	}, payloadOf[engine.PublishedPayload](t, tail[2]))

	pubs := h.runner.callsOf("publish")
	require.Len(t, pubs, 1)

	req := pubs[0].pub
	assert.Equal(t, exportOK.Bundle, req.Bundle, "the bundle the export stored")
	assert.Equal(t, exportOK.HeadCommit, req.Head)
	assert.Equal(t, engine.BranchName(h.task.Title), req.Branch)
	assert.Empty(t, req.Prev)
	assert.Zero(t, req.Number)
	assert.Equal(t, res.Title, req.Title)

	for _, v := range []string{res.Summary, res.WhereToLook[0], verifyOK.Command} {
		assert.Contains(t, req.Body, v)
	}

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.NotNil(t, snap.Facts.PR)

	pr := *snap.Facts.PR
	assert.True(t, now.Equal(pr.Since), "the wait begins at the publish")

	pr.Since = time.Time{}
	assert.Equal(t, engine.PRFact{
		Number: publishedOK.Number, URL: publishedOK.URL, NodeID: publishedOK.NodeID, Branch: publishedOK.Branch,
		Head: publishedOK.Head, Heads: []string{publishedOK.Head},
	}, pr)
}

func TestPublishRejectedStops(t *testing.T) {
	h := newHarness(t)
	h.withPublish()

	const rejected = "! [remote rejected] head456 -> cavu/fix-the-login (protected branch hook declined)"

	h.runner.publishes = []step[engine.PublishResult]{{res: engine.PublishResult{Rejected: rejected}}}

	// The session takes time, so a text that read the clock at the publish would change by the retry.
	h.runner.onCall = func(c call) {
		if c.method == "work" {
			h.clock.Advance(10 * time.Minute)
		}
	}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/keep"}, h.runner.trace())

	stop := h.lastStop()
	assert.Equal(t, engine.StopPushRejected, stop.Reason)
	assert.Equal(t, engine.ClearRetry, stop.Action)
	assert.Contains(t, stop.Explanation, "protected branch hook declined")
	assert.Equal(t, rejected, stop.Detail)
	assert.Empty(t, h.eventsOf(engine.Published))
	assert.Empty(t, h.eventsOf(engine.RunEnded))
	assert.Empty(t, h.spy.ended(), "the run stays open")

	h.clock.Advance(time.Hour)
	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

	before := len(h.runner.calls)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"publish", "release/remove"}, h.runner.trace()[before:])

	pubs := h.runner.callsOf("publish")
	require.Len(t, pubs, 2)
	assert.Equal(t, workDone.Title, pubs[1].pub.Title)
	assert.Equal(t, pubs[0].pub.Title, pubs[1].pub.Title)
	assert.Equal(t, pubs[0].pub.Body, pubs[1].pub.Body)
	assert.Equal(t, pubs[0].runID, pubs[1].runID, "the retry publishes from the same run")
}

func TestPublishFaultIsRetried(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.runner.publishes = []step[engine.PublishResult]{{err: errors.New("GitHub answered 502 Bad Gateway")}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/keep"}, h.runner.trace())

	stop := h.lastStop()
	assert.Equal(t, engine.StopSandbox, stop.Reason)
	assert.Contains(t, stop.Explanation, "502")

	before := len(h.runner.calls)
	h.clock.Advance(30 * time.Second)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"publish", "release/remove"}, h.runner.trace()[before:])
	assert.Len(t, h.eventsOf(engine.Published), 1)
	assert.Equal(t, 1, h.spy.created(), "the retry publishes from the same run")
}

// A publish of a task that has a pull request pushes onto its branch and
// writes no title or text, which a person may have edited.
func TestSecondPublishSendsNoText(t *testing.T) {
	h := newHarness(t)
	h.withPublish()

	bundle := filepath.Join(t.TempDir(), "task.bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("# v2 git bundle of the fix\n"), 0o600))

	exported, err := json.Marshal(engine.ExportedPayload{HeadCommit: "fix789", BundlePath: bundle})
	require.NoError(t, err)

	first := h.seed(
		ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix login","head_commit":"head456"}`),
		ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed","command":"make test"}`),
		ev(engine.ExportStarted, ""), ev(engine.Exported, `{"head_commit":"head456"}`), ev(engine.PublishStarted, ""),
		ev(engine.Published, `{"branch":"cavu/fix-the-login-2","head":"head456","number":7,"url":"https://example.com/web/pull/7","node_id":"PR_7"}`),
		ev(engine.RunEnded, `{"outcome":"exported"}`),
	)

	end := now
	first.Outcome, first.EndedAt = engine.RunExported, &end
	require.NoError(t, h.store.UpdateRun(t.Context(), first))

	h.seed(
		ev(engine.RunStarted, `{"reason":"ci_fix","head":"head456"}`), ev(engine.PrepareStarted, ""),
		ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix the checks","head_commit":"fix789"}`),
		ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed","command":"make test"}`),
		ev(engine.ExportStarted, ""), ev(engine.Exported, string(exported)),
	)

	// What the runner returns for a later publish: no address of the pull request.
	h.runner.publishes = []step[engine.PublishResult]{{res: engine.PublishResult{Branch: "cavu/fix-the-login-2", Head: "fix789", Number: 7}}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"publish", "release/remove"}, h.runner.trace())

	pubs := h.runner.callsOf("publish")
	require.Len(t, pubs, 1)
	assert.Equal(t, engine.PublishRequest{
		Bundle: []byte("# v2 git bundle of the fix\n"), Head: "fix789", Prev: "head456", Branch: "cavu/fix-the-login-2", Number: 7,
	}, pubs[0].pub)

	published := h.eventsOf(engine.Published)
	require.Len(t, published, 2)
	assert.Equal(t, engine.PublishedPayload{
		Branch: "cavu/fix-the-login-2", Head: "fix789", Number: 7, URL: "https://example.com/web/pull/7", NodeID: "PR_7",
	}, payloadOf[engine.PublishedPayload](t, published[1]))

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.NotNil(t, snap.Facts.PR)
	assert.Equal(t, []string{"head456", "fix789"}, snap.Facts.PR.Heads)
}
