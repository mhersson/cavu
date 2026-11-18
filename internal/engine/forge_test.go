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

// watching runs the task with publish on until it watches the pull request it
// opened, publishedOK, at the clock's time.
func (h *harness) watching() {
	h.t.Helper()
	h.withPublish()

	status, err := h.run(h.t.Context())
	require.NoError(h.t, err)
	require.Equal(h.t, engine.StatusInReview, status)
}

// observe has the loop look at the task's pull request, which shows o if the
// loop looks, and returns fast.
func (h *harness) observe(o engine.Observation) bool {
	h.t.Helper()

	h.runner.observes = []step[engine.Observation]{{res: o}}

	fast, err := h.loop.Observe(h.t.Context(), h.task.ID)
	require.NoError(h.t, err)

	return fast
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
	}, pr, "the export it published")
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

// exportedFix seeds a fix run on the pull request publishedRun opened, which
// exported fix789 and did not publish it yet. The runner's next publish returns
// what it returns for a later publish: no address of the pull request.
func (h *harness) exportedFix() {
	h.t.Helper()

	bundle := filepath.Join(h.t.TempDir(), "task.bundle")
	require.NoError(h.t, os.WriteFile(bundle, []byte("# v2 git bundle of the fix\n"), 0o600))

	exported, err := json.Marshal(engine.ExportedPayload{HeadCommit: "fix789", BundlePath: bundle})
	require.NoError(h.t, err)

	h.seed(
		ev(engine.RunStarted, `{"reason":"ci_fix","head":"head456"}`), ev(engine.PrepareStarted, ""),
		ev(engine.Prepared, `{"base_commit":"base123"}`),
		ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`),
		ev(engine.SessionEnded, `{"outcome":"done","changed":true,"title":"Fix the checks","head_commit":"fix789"}`),
		ev(engine.VerifyStarted, ""), ev(engine.VerifyEnded, `{"result":"passed","command":"make test"}`),
		ev(engine.ExportStarted, ""), ev(engine.Exported, string(exported)),
	)

	h.runner.publishes = []step[engine.PublishResult]{{res: engine.PublishResult{Branch: "cavu/fix-the-login-2", Head: "fix789", Number: 7}}}
}

// A publish of a task that has a pull request pushes onto its branch and
// writes no title or text, which a person may have edited.
func TestSecondPublishSendsNoText(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.publishedRun()
	h.exportedFix()

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

// Observations of publishedOK's pull request.
var (
	checksPending = engine.Observation{State: engine.PROpen, Draft: true, Head: "head456", Checks: engine.ChecksPending}
	checksNone    = engine.Observation{State: engine.PROpen, Draft: true, Head: "head456", Checks: engine.ChecksNone}
	checksPassed  = engine.Observation{State: engine.PROpen, Draft: true, Head: "head456", Checks: engine.ChecksPassed}
)

// stopsOf returns the reasons of the task's stops, oldest first.
func (h *harness) stopsOf() []string {
	h.t.Helper()

	var reasons []string
	for _, e := range h.eventsOf(engine.Stopped) {
		reasons = append(reasons, payloadOf[engine.StoppedPayload](h.t, e).Reason)
	}

	return reasons
}

func TestObserveRecordsOnlyAChange(t *testing.T) {
	t.Run("two equal looks record one event, and a change a second", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		h.observe(checksPending)
		h.observe(checksPending)
		require.Len(t, h.eventsOf(engine.PRObserved), 1)

		// A check failed while others still run.
		changed := checksPending
		changed.Failed = []string{"lint"}
		h.observe(changed)

		seen := h.eventsOf(engine.PRObserved)
		require.Len(t, seen, 2)
		assert.Equal(t, checksPending, payloadOf[engine.Observation](t, seen[0]))
		assert.Equal(t, changed, payloadOf[engine.Observation](t, seen[1]))
		assert.Equal(t, "runner", seen[1].Actor)
		assert.Zero(t, seen[1].RunID, "no run is open")

		looks := h.runner.callsOf("observe")
		require.Len(t, looks, 3)

		for _, c := range looks {
			assert.Equal(t, publishedOK.Number, c.number)
		}
	})

	t.Run("an hour of looks at a pull request that does not change records one event", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		for range 360 {
			h.observe(checksPending)
			h.clock.Advance(10 * time.Second)
		}

		assert.Len(t, h.runner.callsOf("observe"), 360)
		assert.Len(t, h.eventsOf(engine.PRObserved), 1)
	})

	// A project's secret can be a plain word that a reviewer writes too. The
	// requests are still seen while the task is stopped for them.
	t.Run("a request is compared as the record holds it, masked", func(t *testing.T) {
		h := newHarness(t)

		p := h.project()
		p.SecretVars = []string{"DB_USER"}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))
		require.NoError(t, h.vault.Set(t.Context(), engine.ProjectSecretName("web", "DB_USER"), "postgres"))
		h.watching()
		h.stopped(engine.StopChangesRequested)

		asked := checksPending
		asked.Comments = []engine.Comment{{ID: "c1", Author: "ana", Path: "db.go", Line: 3, Body: "Connect as postgres here."}}
		h.observe(asked)
		h.observe(asked)

		assert.Len(t, h.runner.callsOf("observe"), 2)

		seen := h.eventsOf(engine.PRObserved)
		require.Len(t, seen, 1)
		assert.NotContains(t, string(seen[0].Payload), "postgres")
	})
}

func TestObserveLosesToAPersonsAction(t *testing.T) {
	h := newHarness(t)
	h.watching()

	rivalled := false
	h.spy.rival = func(int) (store.Event, bool) {
		if rivalled {
			return store.Event{}, false
		}

		rivalled = true

		return store.Event{Type: engine.HumanCancelled, Actor: "user", Payload: json.RawMessage(`{}`)}, true
	}

	h.runner.observes = []step[engine.Observation]{{res: checksPending}}

	_, err := h.loop.Observe(t.Context(), h.task.ID)
	require.NoError(t, err)
	require.True(t, rivalled, "the cancel landed between the snapshot and the append")
	assert.Len(t, h.runner.callsOf("observe"), 1)
	assert.Empty(t, h.eventsOf(engine.PRObserved))
	assert.Len(t, h.eventsOf(engine.HumanCancelled), 1)

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusCancelled, status)
}

// Just after a publish GitHub may still show the head published before.
func TestAnEarlierHeadIsNotATakeover(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.publishedRun()
	h.exportedFix()

	_, err := h.run(t.Context())
	require.NoError(t, err)
	require.Len(t, h.eventsOf(engine.Published), 2)

	earlier := checksPassed
	earlier.Draft = false

	later := checksPending
	later.Draft, later.Head = false, "fix789"

	for _, o := range []engine.Observation{earlier, later} {
		h.observe(o)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.KindWatch, engine.Decide(snap).Kind)
	}

	assert.Len(t, h.eventsOf(engine.PRObserved), 2)
	assert.Empty(t, h.stopsOf())
	assert.Empty(t, h.runner.callsOf("ready"))
}

func TestStoppedTaskStillSeesTheMerge(t *testing.T) {
	merged := engine.Observation{State: engine.PRMerged, Head: "head456", Checks: engine.ChecksNone}

	tests := []struct {
		name  string
		stop  func(h *harness) int64 // stops the task; the run it leaves open, 0 for none
		trace []string
	}{
		{
			name: "no check started",
			stop: func(h *harness) int64 {
				h.stopped(engine.StopNoChecks)

				return 0
			},
			trace: []string{"observe", "release/remove"},
		},
		{
			name: "a fix run asked a question",
			stop: func(h *harness) int64 {
				return h.seed(
					ev(engine.RunStarted, `{"reason":"ci_fix","head":"head456"}`), ev(engine.PrepareStarted, ""),
					ev(engine.Prepared, `{"base_commit":"base123"}`),
					ev(engine.SessionStarted, `{"mode":"fix_ci","session_id":"`+seededSession+`","resume":true}`),
					ev(engine.SessionEnded, `{"outcome":"blocked","questions":["Which linter config?"]}`),
					ev(engine.Stopped, `{"reason":"question","explanation":"Which linter config?","action":"answer"}`),
				).ID
			},
			trace: []string{"observe", "release/run", "release/remove"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.withPublish()
			h.publishedRun()
			runID := tt.stop(h)

			h.observe(merged)

			seen := h.eventsOf(engine.PRObserved)
			require.Len(t, seen, 1)
			assert.Equal(t, runID, seen[0].RunID, "recorded under the open run")

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusDone, status)
			assert.Equal(t, tt.trace, h.runner.trace())
			assert.Len(t, h.eventsOf(engine.HomeRemoved), 1)

			// A task that is over is not looked at again.
			h.observe(merged)
			assert.Len(t, h.runner.callsOf("observe"), 1)
		})
	}
}

func TestClosedPullRequestCancels(t *testing.T) {
	h := newHarness(t)
	h.watching()

	closed := checksPassed
	closed.State = engine.PRClosed
	h.observe(closed)

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusCancelled, status)

	stop := h.lastStop()
	assert.Equal(t, engine.StopCancelled, stop.Reason)
	assert.Contains(t, stop.Explanation, publishedOK.URL)

	assert.Equal(t, []string{"prepare", "work/start", "verify", "export", "publish", "release/run", "observe", "release/remove"}, h.runner.trace())
	assert.Len(t, h.eventsOf(engine.HomeRemoved), 1)

	h.observe(closed)
	assert.Len(t, h.runner.callsOf("observe"), 1, "a cancelled task's pull request is not looked at again")
}

func TestZeroChecks(t *testing.T) {
	t.Run("the grace, one ready-marking, the grace again, then a stop", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		assert.True(t, h.observe(checksNone), "a check may start in the grace")

		h.clock.Advance(3*time.Minute - time.Second)
		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Empty(t, h.runner.callsOf("ready"))

		h.clock.Advance(time.Second)
		_, err = h.run(t.Context())
		require.NoError(t, err)

		readies := h.runner.callsOf("ready")
		require.Len(t, readies, 1)
		assert.Equal(t, publishedOK.NodeID, readies[0].nodeID)

		marked := h.events()[len(h.events())-2:]
		assert.Equal(t, []string{engine.ReadyStarted, engine.PRReady}, types(marked))
		assert.Equal(t, []int64{0, 0}, []int64{marked[0].RunID, marked[1].RunID}, "no run is open")

		ready := checksNone
		ready.Draft = false
		assert.True(t, h.observe(ready), "the grace starts again at the ready-marking")

		h.clock.Advance(3*time.Minute - time.Second)
		_, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Empty(t, h.stopsOf())

		h.clock.Advance(time.Second)
		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Equal(t, []string{engine.StopNoChecks}, h.stopsOf())
		assert.Contains(t, h.lastStop().Explanation, publishedOK.URL)

		// A retry waits a new grace and marks nothing again.
		h.clock.Advance(time.Hour)
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		h.clock.Advance(3*time.Minute - time.Second)
		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		h.clock.Advance(time.Second)
		_, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{engine.StopNoChecks, engine.StopNoChecks}, h.stopsOf())
		assert.Len(t, h.runner.callsOf("ready"), 1, "one ready-marking")
	})

	t.Run("a project without CI is marked ready at once", func(t *testing.T) {
		h := newHarness(t)

		p := h.project()
		p.Forge.NoCI = true
		require.NoError(t, h.store.UpdateProject(t.Context(), p))
		h.watching()

		h.observe(checksNone)

		_, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Len(t, h.runner.callsOf("ready"), 1)

		ready := checksNone
		ready.Draft = false
		h.observe(ready)
		h.clock.Advance(time.Hour)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Empty(t, h.stopsOf())
		assert.Len(t, h.runner.callsOf("ready"), 1)
	})
}

func TestCITimeout(t *testing.T) {
	tests := []struct {
		name, setting string
		timeout       time.Duration // 0: never
	}{
		{name: "at the default", timeout: 120 * time.Minute},
		{name: "at a set value", setting: "30", timeout: 30 * time.Minute},
		{name: "never at 0", setting: "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.setting != "" {
				h.set("ci_timeout_minutes", tt.setting)
			}

			h.watching()
			h.observe(checksPending)
			require.Len(t, h.eventsOf(engine.PRObserved), 1)

			if tt.timeout == 0 {
				h.clock.Advance(30 * 24 * time.Hour)

				status, err := h.run(t.Context())
				require.NoError(t, err)
				assert.Equal(t, engine.StatusInReview, status)
				assert.Empty(t, h.stopsOf())

				return
			}

			h.clock.Advance(tt.timeout - time.Second)
			_, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Empty(t, h.stopsOf())

			h.clock.Advance(time.Second)
			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, engine.StatusBlocked, status)
			assert.Equal(t, []string{engine.StopCITimeout}, h.stopsOf())
			assert.Contains(t, h.lastStop().Explanation, publishedOK.URL)
		})
	}
}

func TestObserveIsFastWhileChecksAreDue(t *testing.T) {
	tests := []struct {
		name   string
		checks string
		after  time.Duration // since the publish
		fast   bool
	}{
		{name: "checks pending", checks: engine.ChecksPending, after: time.Hour, fast: true},
		{name: "no check inside the grace", checks: engine.ChecksNone, after: 3*time.Minute - time.Second, fast: true},
		{name: "no check after the grace", checks: engine.ChecksNone, after: 3 * time.Minute},
		{name: "checks passed", checks: engine.ChecksPassed},
		{name: "checks failed", checks: engine.ChecksFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.watching()
			h.clock.Advance(tt.after)

			o := checksPending
			o.Checks = tt.checks

			assert.Equal(t, tt.fast, h.observe(o))
			assert.Len(t, h.runner.callsOf("observe"), 1)
		})
	}
}

// A ready-marking runs with no run open: its fault stops the task with no run,
// releases nothing, and is retried as any fault is.
func TestReadyFaultIsRetried(t *testing.T) {
	t.Run("the runner fails", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.observe(checksPassed)
		h.runner.readies = []step[struct{}]{{err: errors.New("GitHub answered 502 Bad Gateway")}}

		before := len(h.runner.calls)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Equal(t, []string{"ready"}, h.runner.trace()[before:], "no run is released")

		stops := h.eventsOf(engine.Stopped)
		require.Len(t, stops, 1)
		assert.Zero(t, stops[0].RunID)
		assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
		assert.Contains(t, h.lastStop().Explanation, "502")

		h.clock.Advance(30 * time.Second)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"ready", "ready"}, h.runner.trace()[before:])
		assert.Len(t, h.eventsOf(engine.PRReady), 1)
	})

	t.Run("its config cannot be built", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.observe(checksPassed)

		p := h.project()
		p.SecretVars = []string{"NPM_TOKEN"}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))

		before := len(h.runner.calls)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Len(t, h.runner.calls, before, "nothing of the runner runs")

		stops := h.eventsOf(engine.Stopped)
		require.Len(t, stops, 1)
		assert.Zero(t, stops[0].RunID)
		assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
		assert.Contains(t, h.lastStop().Explanation, "project.web.NPM_TOKEN")
	})
}

// A look at the pull request and its ready-marking come every few seconds, so
// their config holds none of the instance's files, which a run's config holds.
func TestTheWatchReadsNoInstanceFiles(t *testing.T) {
	h := newHarness(t)

	root := t.TempDir()
	instructions, skills := filepath.Join(root, "AGENTS.md"), filepath.Join(root, "skills")
	writeFile(t, instructions, "Work in small commits.\n", 0o644)
	writeFile(t, filepath.Join(skills, "one", "SKILL.md"), "one\n", 0o644)
	h.set("instructions_path", instructions)
	h.set("skills_path", skills)

	// A branch other than the title's, which the forge calls must take from the pull request.
	pub := publishedOK
	pub.Branch = "cavu/fix-the-login-2"
	h.runner.publishes = []step[engine.PublishResult]{{res: pub}}

	h.watching()
	h.observe(checksPassed)

	_, err := h.run(t.Context())
	require.NoError(t, err)

	pubs := h.runner.callsOf("publish")
	require.Len(t, pubs, 1)
	assert.Equal(t, "Work in small commits.\n", pubs[0].cfg.Agent.Instance.Instructions)
	assert.Contains(t, pubs[0].cfg.Agent.Instance.Skills, "one/SKILL.md")

	looks, readies := h.runner.callsOf("observe"), h.runner.callsOf("ready")
	require.Len(t, looks, 1)
	require.Len(t, readies, 1)
	assert.Equal(t, publishedOK.NodeID, readies[0].nodeID)

	for _, c := range []call{looks[0], readies[0]} {
		assert.Zero(t, c.cfg.Agent.Instance, "%s", c)
		assert.Equal(t, pub.Branch, c.cfg.Repo.Branch, "%s", c)
		assert.Equal(t, h.task.ID, c.cfg.TaskID, "%s", c)
		assert.Equal(t, "https://example.com/web.git", c.cfg.Repo.URL, "%s", c)
	}
}
