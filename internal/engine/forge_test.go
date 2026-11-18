package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
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

	// The publish takes time, so its result is recorded later than the export and the claim.
	h.runner.onCall = func(c call) {
		if c.method == "publish" {
			h.clock.Advance(5 * time.Minute)
		}
	}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/run"}, h.runner.trace())

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
	assert.True(t, now.Add(5*time.Minute).Equal(pr.Since), "the wait begins when the publish is recorded, at %s", pr.Since)

	pr.Since = time.Time{}
	assert.Equal(t, engine.PRFact{
		Number: publishedOK.Number, URL: publishedOK.URL, NodeID: publishedOK.NodeID, Branch: publishedOK.Branch,
		Head: publishedOK.Head, Heads: []string{publishedOK.Head}, Export: payloadOf[engine.ExportedPayload](t, tail[0]),
	}, pr, "the export it published is the change the pull request holds")
}

func TestPublishBodyHoldsNoCostOfTheTask(t *testing.T) {
	h := newHarness(t)
	h.withPublish()

	res := workDone
	res.CostEstimateUSD = 987.65
	res.Usage = []store.Usage{{Source: "proxy", Model: "model-a", Input: 1, Output: 1, CostEstimateUSD: 987.65}}
	h.runner.works = []step[engine.WorkResult]{{res: res}}

	_, err := h.run(t.Context())
	require.NoError(t, err)

	totals, err := h.store.UsageTotals(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.InDelta(t, 987.65, totals[0].CostEstimateUSD, 1e-9, "the task's usage is on record")

	pubs := h.runner.callsOf("publish")
	require.Len(t, pubs, 1)
	assert.NotContains(t, pubs[0].pub.Body, "987")
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
	assert.Equal(t, []string{"publish", "release/run"}, h.runner.trace()[before:])

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
	assert.Equal(t, []string{"publish", "release/run"}, h.runner.trace()[before:])
	assert.Len(t, h.eventsOf(engine.Published), 1)
	assert.Equal(t, 1, h.spy.created(), "the retry publishes from the same run")
}

// publishedRun seeds the task's first run, which published head456 as pull
// request 7 and ended.
func (h *harness) publishedRun() {
	h.t.Helper()

	first := h.seed(
		ev(engine.RunStarted, `{"reason":"initial"}`), ev(engine.PrepareStarted, ""), ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"start","session_id":"`+seededSession+`"}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix login","head_commit":"head456"}`),
		ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed","command":"make test"}`),
		ev(engine.ExportStarted, ""), ev(engine.Exported, `{"head_commit":"head456"}`), ev(engine.PublishStarted, ""),
		ev(engine.Published, `{"branch":"cavu/fix-the-login-2","head":"head456","number":7,"url":"https://example.com/web/pull/7","node_id":"PR_7"}`),
		ev(engine.RunEnded, `{"outcome":"exported","kept_home":true}`),
	)

	end := now
	first.Outcome, first.EndedAt = engine.RunExported, &end
	require.NoError(h.t, h.store.UpdateRun(h.t.Context(), first))
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

	h.publishedRun()
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
	assert.Equal(t, []string{"publish", "release/run"}, h.runner.trace())

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

// A task whose pull request is watched rests: RunTask calls no runner and records nothing.
func TestRunTaskRestsAtTheWatch(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.publishedRun()

	before := len(h.events())

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Empty(t, h.runner.calls)
	assert.Len(t, h.events(), before)

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.Equal(t, engine.KindWatch, engine.Decide(snap).Kind)
}

func TestSnapshotReadsTheWatchPolicy(t *testing.T) {
	h := newHarness(t)

	snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.False(t, snap.Watch.NoCI)
	assert.Equal(t, 120*time.Minute, snap.Watch.CITimeout, "ci_timeout_minutes at its default")

	p := h.project()
	p.Forge.NoCI = true
	require.NoError(t, h.store.UpdateProject(t.Context(), p))
	require.NoError(t, h.store.SetSetting(t.Context(), "ci_timeout_minutes", "30"))

	snap, err = h.loop.Snapshot(t.Context(), h.task.ID)
	require.NoError(t, err)
	assert.True(t, snap.Watch.NoCI)
	assert.Equal(t, 30*time.Minute, snap.Watch.CITimeout)
}

// A fix run whose branch holds a head CAVU did not push ends before its
// publish: its export cannot descend from the head a retry takes back.
func TestATakeoverEndsTheOpenRun(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.publishedRun()

	run := h.seed(
		ev(engine.RunStarted, `{"reason":"ci_fix","head":"head456"}`), ev(engine.PrepareStarted, ""),
		ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true,"head_commit":"fix789"}`),
		ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed","command":"make test"}`),
		ev(engine.ExportStarted, ""), ev(engine.Exported, `{"head_commit":"fix789"}`),
	)
	h.add(run.ID, engine.PRObserved, "runner", `{"state":"open","head":"theirs","checks":"pending"}`)

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"release/run"}, h.runner.trace(), "nothing is pushed over the person's commit")

	stop := h.lastStop()
	assert.Equal(t, engine.StopTakenOver, stop.Reason)
	assert.Contains(t, stop.Explanation, "https://example.com/web/pull/7")

	ends := h.eventsOf(engine.RunEnded)
	require.NotEmpty(t, ends)
	assert.Equal(t, engine.RunEndedPayload{Outcome: engine.StopTakenOver, KeptHome: true}, payloadOf[engine.RunEndedPayload](t, ends[len(ends)-1]))
	assert.Equal(t, map[int64]string{run.ID: engine.StopTakenOver}, h.spy.ended())
}

// A run whose work is over with no export ends as finished, not exported.
func TestARunEndedWithoutAnExportIsFinished(t *testing.T) {
	tests := []struct {
		name   string
		seen   string // the observation recorded before the fix run's start
		more   []store.Event
		merged string // the observation recorded last, "" for none
		status engine.Status
		trace  []string
		stop   string // the stop the watch then records, "" for none
	}{
		{
			name: "a fix run that changed nothing",
			seen: `{"state":"open","draft":true,"head":"head456","checks":"failed","failed":["lint"]}`,
			more: []store.Event{
				ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`),
				ev(engine.SessionEnded, `{"outcome":"done","head_commit":"head456"}`),
			},
			status: engine.StatusBlocked, trace: []string{"release/run"}, stop: engine.StopCIFailed,
		},
		{
			name:   "a merge while a fix run works",
			seen:   `{"state":"open","draft":true,"head":"head456","checks":"failed","failed":["lint"]}`,
			more:   []store.Event{ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`)},
			merged: `{"state":"merged","head":"head456","checks":"failed","failed":["lint"]}`,
			status: engine.StatusDone, trace: []string{"release/run", "release/remove"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.withPublish()
			h.publishedRun()
			h.add(0, engine.PRObserved, "runner", tt.seen)

			run := h.seed(append([]store.Event{
				ev(engine.RunStarted, `{"reason":"ci_fix","head":"head456"}`), ev(engine.PrepareStarted, ""),
				ev(engine.Prepared, `{"base_commit":"base123"}`),
			}, tt.more...)...)

			if tt.merged != "" {
				h.add(run.ID, engine.PRObserved, "runner", tt.merged)
			}

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, tt.trace, h.runner.trace())

			ends := h.eventsOf(engine.RunEnded)
			require.NotEmpty(t, ends)

			end := payloadOf[engine.RunEndedPayload](t, ends[len(ends)-1])
			assert.Equal(t, engine.RunFinished, end.Outcome)
			assert.True(t, end.KeptHome)
			assert.Equal(t, map[int64]string{run.ID: engine.RunFinished}, h.spy.ended())

			var stops []string
			for _, e := range h.eventsOf(engine.Stopped) {
				stops = append(stops, payloadOf[engine.StoppedPayload](t, e).Reason)
			}

			if tt.stop == "" {
				assert.Empty(t, stops)
			} else {
				assert.Equal(t, []string{tt.stop}, stops, "a fix run that changed nothing is no stop; the watch's own follows")
			}
		})
	}
}
