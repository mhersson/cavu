package engine_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/prompts"
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
	assert.Equal(t, []string{"observe", "publish", "release/run"}, h.runner.trace(), "a look at the pull request comes first")

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
	assert.Contains(t, stop.Explanation, "theirs", "the commit the look found")

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
	checksFailed  = engine.Observation{
		State: engine.PROpen, Draft: true, Head: "head456", Checks: engine.ChecksFailed, Failed: []string{"lint"},
	}
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

	t.Run("the first look after a person's action is recorded though it is equal, and the next hour's are not", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.observe(checksPending)
		h.stopped(engine.StopCITimeout)
		h.observe(checksPending)
		require.Len(t, h.eventsOf(engine.PRObserved), 1)

		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		for range 360 {
			h.observe(checksPending)
			h.clock.Advance(10 * time.Second)
		}

		assert.Len(t, h.runner.callsOf("observe"), 362)

		seen := h.eventsOf(engine.PRObserved)
		require.Len(t, seen, 2)
		assert.Equal(t, checksPending, payloadOf[engine.Observation](t, seen[1]))
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

// The poller waits out a rate limit, so it reaches Observe's caller as the runner returned it.
func TestObserveReturnsARateLimit(t *testing.T) {
	h := newHarness(t)
	h.watching()

	until := now.Add(time.Hour)
	h.runner.observes = []step[engine.Observation]{{err: &engine.RateLimited{Until: until}}}
	before := len(h.events())

	fast, err := h.loop.Observe(t.Context(), h.task.ID)

	var limited *engine.RateLimited
	require.ErrorAs(t, err, &limited)
	assert.True(t, until.Equal(limited.Until))
	assert.False(t, fast)
	assert.Len(t, h.runner.callsOf("observe"), 1)
	assert.Len(t, h.events(), before, "nothing is recorded")
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

	// The checks that failed on the earlier head, which the fix run answered.
	earlier := checksPassed
	earlier.Draft, earlier.Checks, earlier.Failed = false, engine.ChecksFailed, []string{"lint"}

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
	assert.Empty(t, h.runner.callsOf("prepare"), "no new fix run")
}

// A person acts on GitHub or on the project and retries before CAVU's next
// look, which is what the retry is decided on.
func TestARetryIsDecidedOnALookAfterIt(t *testing.T) {
	// retried has a person retry the task's stop, and checks that nothing acts before a look.
	retried := func(t *testing.T, h *harness) {
		t.Helper()
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		before := len(h.runner.calls)
		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Empty(t, h.runner.trace()[before:], "nothing acts on the look before the retry")
	}

	t.Run("failed checks the project ignores since", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.runner.works = []step[engine.WorkResult]{unchangedAt("head456")}

		macos := checksFailed
		macos.Failed = []string{"macos"}
		assert.Equal(t, engine.StatusBlocked, h.lookAt(macos))
		require.Equal(t, []string{engine.StopCIFailed}, h.stopsOf())

		p := h.project()
		p.Forge.IgnoredChecks = []string{"macos"}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))
		retried(t, h)

		// GitHub no longer counts the check the project ignores.
		before := len(h.runner.calls)
		assert.Equal(t, engine.StatusInReview, h.lookAt(checksPassed))
		assert.Equal(t, []string{"observe", "ready"}, h.runner.trace()[before:])
		assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456")}, h.runStarts(), "no fix run on the old failure")
		assert.Equal(t, []string{engine.StopCIFailed}, h.stopsOf())
	})

	t.Run("a conflict resolved since the last look", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		conflicted := checksPassed
		conflicted.Conflict = true
		assert.Equal(t, engine.StatusBlocked, h.lookAt(conflicted))
		retried(t, h)

		resolved := checksPending
		resolved.Head = "merge789"
		assert.Equal(t, engine.StatusInReview, h.lookAt(resolved))
		assert.Equal(t, []string{engine.StopConflict}, h.stopsOf(), "one retry and one look")

		// CAVU goes on from the person's commit.
		h.fixes("fix1")

		failed := checksFailed
		failed.Head = "merge789"
		assert.Equal(t, engine.StatusInReview, h.lookAt(failed))
		assert.Equal(t, "merge789", h.lastCall("prepare").cfg.Repo.Head)
		assert.Equal(t, []string{engine.StopConflict}, h.stopsOf())
	})

	t.Run("a takeover gives CAVU the head the look shows", func(t *testing.T) {
		h := newHarness(t)
		h.watching()

		theirs := checksPending
		theirs.Head = "theirs1"
		assert.Equal(t, engine.StatusBlocked, h.lookAt(theirs))
		require.Equal(t, []string{engine.StopTakenOver}, h.stopsOf())

		// The person pushed again after the last look.
		retried(t, h)

		theirs.Head = "theirs2"
		assert.Equal(t, engine.StatusInReview, h.lookAt(theirs))
		assert.Equal(t, []string{engine.StopTakenOver}, h.stopsOf())

		snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		require.NotNil(t, snap.Facts.PR)
		assert.Equal(t, "theirs2", snap.Facts.PR.Head)
		assert.Equal(t, []string{publishedOK.Head, "theirs2"}, snap.Facts.PR.Heads)
	})
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
		h.observe(ready)

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
		stop   string        // an open stop's reason, "" for none
		noCI   bool
		fast   bool
	}{
		{name: "checks pending", checks: engine.ChecksPending, after: time.Hour, fast: true},
		{name: "no check inside the grace", checks: engine.ChecksNone, after: 3*time.Minute - time.Second, fast: true},
		{name: "no check after the grace", checks: engine.ChecksNone, after: 3 * time.Minute},
		{name: "checks passed", checks: engine.ChecksPassed},
		{name: "checks failed", checks: engine.ChecksFailed},
		{name: "checks pending under a stop", checks: engine.ChecksPending, after: 3 * time.Hour, stop: engine.StopCITimeout},
		{name: "no check inside the grace under a stop", checks: engine.ChecksNone, after: time.Minute, stop: engine.StopConflict},
		{name: "no check inside the grace on a project without CI", checks: engine.ChecksNone, after: time.Minute, noCI: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if tt.noCI {
				p := h.project()
				p.Forge.NoCI = true
				require.NoError(t, h.store.UpdateProject(t.Context(), p))
			}

			h.watching()
			h.clock.Advance(tt.after)

			if tt.stop != "" {
				h.stopped(tt.stop)
			}

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

// A look at the pull request, which comes every few seconds, and the
// ready-marking build their config without the instance's files, which a run's
// config holds.
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

// fixes scripts the runner's next sessions, exports and publishes: each
// session moves the task's branch to the next of heads, which its export and
// its publish take.
func (h *harness) fixes(heads ...string) {
	for _, head := range heads {
		w, e, p := workDone, exportOK, publishedOK
		w.HeadCommit, e.HeadCommit, p.Head = head, head, head
		h.runner.works = append(h.runner.works, step[engine.WorkResult]{res: w})
		h.runner.exports = append(h.runner.exports, step[engine.ExportResult]{res: e})
		h.runner.publishes = append(h.runner.publishes, step[engine.PublishResult]{res: p})
	}
}

// unchangedAt is a session that ended done and left the task's branch at head.
func unchangedAt(head string) step[engine.WorkResult] {
	return step[engine.WorkResult]{res: engine.WorkResult{Outcome: engine.OutcomeDone, Summary: "Nothing to change.", HeadCommit: head}}
}

// askedAt is publishedOK's pull request at head, its checks running, with a
// reviewer's request for each of ids.
func askedAt(head string, ids ...string) engine.Observation {
	o := checksPending
	o.Head = head

	for _, id := range ids {
		o.Comments = append(o.Comments, engine.Comment{ID: id, Author: "ana", Path: "login.go", Line: 12, Body: "Request " + id + "."})
	}

	return o
}

// lookAt has the loop look at the task's pull request, which shows o, then
// runs the task until it rests, and returns its status.
func (h *harness) lookAt(o engine.Observation) engine.Status {
	h.t.Helper()
	h.observe(o)

	status, err := h.run(h.t.Context())
	require.NoError(h.t, err)

	return status
}

// runStarts returns the payloads of the task's run starts, oldest first.
func (h *harness) runStarts() []engine.RunStartedPayload {
	h.t.Helper()

	var out []engine.RunStartedPayload
	for _, e := range h.eventsOf(engine.RunStarted) {
		out = append(out, payloadOf[engine.RunStartedPayload](h.t, e))
	}

	return out
}

// lastCall returns the newest call of method.
func (h *harness) lastCall(method string) call {
	h.t.Helper()

	calls := h.runner.callsOf(method)
	require.NotEmpty(h.t, calls, "a %s call", method)

	return calls[len(calls)-1]
}

func ciFixFrom(head string) engine.RunStartedPayload {
	return engine.RunStartedPayload{Reason: engine.ReasonCIFix, Head: head}
}

func commentsFrom(head string, requests ...string) engine.RunStartedPayload {
	return engine.RunStartedPayload{Reason: engine.ReasonComments, Head: head, Requests: requests}
}

var initialRun = engine.RunStartedPayload{Reason: "initial"}

// A fix run that changed nothing ends without a stop of its own; the watch
// then stops the task, since the checks still fail on a head a fix run had.
func TestAFixRunThatChangesNothingEnds(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.runner.works = []step[engine.WorkResult]{unchangedAt("head456"), unchangedAt("head456")}

	failed := checksFailed
	failed.Failed = []string{"lint", "test"}

	before := len(h.runner.calls)
	assert.Equal(t, engine.StatusBlocked, h.lookAt(failed))
	assert.Equal(t, []string{"observe", "prepare", "work/fix_ci", "release/run"}, h.runner.trace()[before:])

	ends := h.eventsOf(engine.RunEnded)
	end := payloadOf[engine.RunEndedPayload](t, ends[len(ends)-1])
	assert.Equal(t, engine.RunFinished, end.Outcome)
	assert.True(t, end.KeptHome)

	assert.Equal(t, []string{engine.StopCIFailed}, h.stopsOf(), "no no_change")

	for _, datum := range []string{"lint", "test", publishedOK.URL} {
		assert.Contains(t, h.lastStop().Explanation, datum)
	}

	h.clock.Advance(time.Hour)
	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	h.observe(failed)

	before = len(h.runner.calls)
	_, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"prepare", "work/fix_ci", "release/run"}, h.runner.trace()[before:], "a retry starts a new fix run")
	assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456"), ciFixFrom("head456")}, h.runStarts())
}

// A follow-up run answers the reviewers with one comment on the pull request
// once its work is over; then the run ends and the watch goes on.
func TestAFollowUpRunAnswersOnThePullRequest(t *testing.T) {
	// comment is what CAVU writes for the task after a session that ended with
	// summary, having moved the head or not.
	comment := func(t *testing.T, h *harness, summary string, changed bool) string {
		t.Helper()

		body, err := prompts.Render(prompts.Reply, prompts.ReplyData{TaskID: h.task.ID, Summary: summary, Changed: changed})
		require.NoError(t, err)

		return body
	}

	// tail is the types of the task's newest n events.
	tail := func(h *harness, n int) []string {
		evs := h.events()

		return types(evs[len(evs)-n:])
	}

	t.Run("a run that moved the head, then one that did not", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.fixes("r1")

		const moved = "Renamed the handler as asked, token-r1."

		h.runner.works[0].res.Summary = moved

		assert.Equal(t, engine.StatusInReview, h.lookAt(askedAt("head456", "c1")))
		assert.Equal(t, []string{engine.Published, engine.ReplyStarted, engine.Replied, engine.RunEnded}, tail(h, 4))

		reply := h.lastCall("reply")
		assert.Equal(t, publishedOK.Number, reply.number)
		assert.Equal(t, h.lastCall("prepare").runID, reply.runID, "under the follow-up run")
		assert.Contains(t, reply.body, "token-r1")
		assert.Equal(t, comment(t, h, moved, true), reply.body, "the task's id, the summary, and a head that moved")

		const kept = "Kept the timeout at 30 seconds, token-kept: the client retries."

		h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeDone, Summary: kept, HeadCommit: "r1"}}}

		before := len(h.runner.calls)
		assert.Equal(t, engine.StatusInReview, h.lookAt(askedAt("r1", "c1", "c2")))
		assert.Equal(t, []string{"observe", "prepare", "work/comments", "reply", "release/run"}, h.runner.trace()[before:])
		assert.Equal(t, []string{engine.SessionEnded, engine.ReplyStarted, engine.Replied, engine.RunEnded}, tail(h, 4))
		assert.Contains(t, h.lastCall("reply").body, "token-kept")
		assert.Equal(t, comment(t, h, kept, false), h.lastCall("reply").body, "a head that did not move")

		assert.Empty(t, h.stopsOf())

		snap, err := h.loop.Snapshot(t.Context(), h.task.ID)
		require.NoError(t, err)
		assert.Equal(t, engine.KindWatch, engine.Decide(snap).Kind)
		assert.Len(t, h.eventsOf(engine.Replied), 2, "one answer for each run")
	})

	t.Run("a reply that failed is retried, and answers once", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.fixes("r1")
		h.runner.replies = []step[struct{}]{{err: errors.New("GitHub answered 502 Bad Gateway")}}

		assert.Equal(t, engine.StatusBlocked, h.lookAt(askedAt("head456", "c1")))
		assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
		assert.Contains(t, h.lastStop().Explanation, "502")
		assert.Empty(t, h.eventsOf(engine.Replied))
		assert.NotContains(t, h.spy.ended(), h.lastCall("reply").runID, "the run stays open")

		h.clock.Advance(30 * time.Second)

		before := len(h.runner.calls)
		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"reply", "release/run"}, h.runner.trace()[before:])
		assert.Len(t, h.eventsOf(engine.Replied), 1)

		replies := h.runner.callsOf("reply")
		require.Len(t, replies, 2)
		assert.Equal(t, replies[0].body, replies[1].body)
	})

	t.Run("a fix run on failed checks gets none", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.fixes("fix1")

		assert.Equal(t, engine.StatusInReview, h.lookAt(checksFailed))
		assert.Len(t, h.eventsOf(engine.Published), 2, "the fix run published")
		assert.Empty(t, h.runner.callsOf("reply"))
		assert.Empty(t, h.eventsOf(engine.ReplyStarted))
	})
}

// A failure is acted on once, by the head it failed on.
func TestAFailureIsActedOnOnce(t *testing.T) {
	t.Run("a fix run that left the head as it was gets no second one on it", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.runner.works = []step[engine.WorkResult]{unchangedAt("head456")}

		assert.Equal(t, engine.StatusBlocked, h.lookAt(checksFailed))
		assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456")}, h.runStarts(), "one of the two fix runs the cap allows")
		assert.Equal(t, []string{engine.StopCIFailed}, h.stopsOf())
	})

	t.Run("the head a fix run pushed gets a fix run of its own", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.fixes("fix1")

		assert.Equal(t, engine.StatusInReview, h.lookAt(checksFailed))

		// GitHub still shows the head before the push, with the failure the fix run answered.
		assert.Equal(t, engine.StatusInReview, h.lookAt(checksFailed))
		assert.Len(t, h.runStarts(), 2)

		failed := checksFailed
		failed.Head = "fix1"
		h.runner.works = []step[engine.WorkResult]{unchangedAt("fix1")}
		h.lookAt(failed)

		assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456"), ciFixFrom("fix1")}, h.runStarts())
		assert.Equal(t, "fix1", h.lastCall("prepare").cfg.Repo.Head)
	})
}

func TestCIFixIsCapped(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.fixes("fix1", "fix2")

	failed := checksFailed
	for _, head := range []string{"head456", "fix1"} {
		failed.Head = head
		assert.Equal(t, engine.StatusInReview, h.lookAt(failed), "a fix run from %s", head)
	}

	failed.Head = "fix2"
	assert.Equal(t, engine.StatusBlocked, h.lookAt(failed))
	assert.Equal(t, []string{engine.StopCIFailed}, h.stopsOf())
	assert.Contains(t, h.lastStop().Explanation, "lint")
	assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456"), ciFixFrom("fix1")}, h.runStarts())

	// A retry allows two more.
	h.fixes("fix3")
	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	h.observe(failed)

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, ciFixFrom("fix2"), h.runStarts()[3])
}

func TestCommentRunsAreCapped(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.fixes("r1", "r2", "r3")

	ids := []string{"c1", "c2", "c3", "c4"}
	heads := []string{"head456", "r1", "r2", "r3"}

	for i := range 3 {
		assert.Equal(t, engine.StatusInReview, h.lookAt(askedAt(heads[i], ids[:i+1]...)), "a follow-up run from %s", heads[i])
	}

	assert.Equal(t, engine.StatusBlocked, h.lookAt(askedAt("r3", ids...)))
	assert.Equal(t, []string{engine.StopChangesRequested}, h.stopsOf())
	assert.Contains(t, h.lastStop().Explanation, publishedOK.URL)
	assert.Equal(t, []engine.RunStartedPayload{
		initialRun, commentsFrom("head456", "c1"), commentsFrom("r1", "c2"), commentsFrom("r2", "c3"),
	}, h.runStarts())

	// A retry allows three more, and the next run takes the request the cap held.
	h.fixes("r4")
	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
	h.observe(askedAt("r3", ids...))

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, commentsFrom("r3", "c4"), h.runStarts()[4])
}

// A reviewer's request is acted on once, by its id.
func TestARequestIsActedOnOnce(t *testing.T) {
	h := newHarness(t)
	h.watching()

	seen := func(head string, requests ...engine.Comment) engine.Observation {
		o := checksPending
		o.Head, o.Comments = head, requests

		return o
	}
	given := func() []engine.Comment { return h.lastCall("work").work.Comments }

	thread := engine.Comment{ID: "c1", Author: "ana", Path: "db.go", Line: 3, Body: "Use a connection pool."}

	h.fixes("r1")
	assert.Equal(t, engine.StatusInReview, h.lookAt(seen("head456", thread)))
	assert.Equal(t, []engine.Comment{thread}, given())

	// The answered thread stays unresolved after CAVU's push.
	assert.Equal(t, engine.StatusInReview, h.lookAt(seen("r1", thread)))
	assert.Len(t, h.runStarts(), 2, "an answered request starts nothing")

	// The reviewer replies in the thread, whose newest comment is a new request;
	// another comes while the run works, and the look before its push shows it.
	reply := engine.Comment{ID: "c1b", Author: "ana", Path: "db.go", Line: 3, Body: "The pool is still opened per request."}
	late := engine.Comment{ID: "c2", Author: "ben", Path: "api.go", Line: 9, Body: "Answer 404 here."}

	h.fixes("r2")
	h.observe(seen("r1", reply))
	h.runner.observes = []step[engine.Observation]{{res: seen("r1", reply, late)}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []engine.Comment{reply}, given(), "the run is given the thread again")

	// The next run takes the request that came while the run before worked; its
	// session fails, and a person's retry resumes it with no request given again.
	h.runner.works = []step[engine.WorkResult]{{res: engine.WorkResult{Outcome: engine.OutcomeFailed, Summary: "The test database is down."}}}
	h.fixes("r3")
	assert.Equal(t, engine.StatusBlocked, h.lookAt(seen("r2", reply, late)))
	assert.Equal(t, []engine.Comment{late}, given())
	assert.Equal(t, engine.StopAgentError, h.lastStop().Reason)

	require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)

	resumed := h.lastCall("work").work
	assert.Equal(t, engine.ModeContinue, resumed.Mode)
	assert.Empty(t, resumed.Comments)

	assert.Equal(t, engine.StatusInReview, h.lookAt(seen("r3", reply, late)))
	assert.Equal(t, []engine.RunStartedPayload{
		initialRun, commentsFrom("head456", "c1"), commentsFrom("r1", "c1b"), commentsFrom("r2", "c2"),
	}, h.runStarts())
}

func TestMergedWhileAFixRunIsOpenEndsTheRun(t *testing.T) {
	h := newHarness(t)
	h.watching()
	h.fixes("fix1")
	h.observe(checksFailed)

	// A person merges while the fix run works, and the look before its push sees it.
	merged := checksFailed
	merged.State = engine.PRMerged
	h.runner.observes = []step[engine.Observation]{{res: merged}}

	before := len(h.runner.calls)
	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusDone, status)
	assert.Equal(t, []string{"prepare", "work/fix_ci", "verify", "export", "observe", "release/run", "release/remove"}, h.runner.trace()[before:])
	assert.Len(t, h.runner.callsOf("publish"), 1, "only the first run's")

	run := h.lastCall("prepare").runID
	seen := h.eventsOf(engine.PRObserved)
	assert.Equal(t, merged, payloadOf[engine.Observation](t, seen[len(seen)-1]))
	assert.Equal(t, run, seen[len(seen)-1].RunID, "recorded under the fix run")
	assert.Contains(t, h.spy.ended(), run)
	assert.Len(t, h.eventsOf(engine.HomeRemoved), 1)
	assert.Empty(t, h.stopsOf())
}

// The publish of a fix run looks at the pull request before it pushes.
func TestPublishDoesNotPushToAMergedPullRequest(t *testing.T) {
	tests := []struct {
		name   string
		state  string
		status engine.Status
		stop   string
	}{
		{name: "merged", state: engine.PRMerged, status: engine.StatusDone},
		{name: "closed", state: engine.PRClosed, status: engine.StatusCancelled, stop: engine.StopCancelled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.withPublish()
			h.publishedRun()
			h.exportedFix()

			look := checksPassed
			look.State = tt.state
			h.runner.observes = []step[engine.Observation]{{res: look}}

			status, err := h.run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.status, status)
			assert.Equal(t, []string{"observe", "release/run", "release/remove"}, h.runner.trace(), "nothing is pushed")

			run := h.lastCall("observe").runID
			seen := h.eventsOf(engine.PRObserved)
			require.Len(t, seen, 1)
			assert.Equal(t, run, seen[0].RunID)
			assert.Equal(t, look, payloadOf[engine.Observation](t, seen[0]))
			assert.Contains(t, h.spy.ended(), run)

			if tt.stop == "" {
				assert.Empty(t, h.stopsOf())

				return
			}

			assert.Equal(t, []string{tt.stop}, h.stopsOf())
			assert.Contains(t, h.lastStop().Explanation, "https://example.com/web/pull/7")
		})
	}

	t.Run("a head CAVU never published", func(t *testing.T) {
		h := newHarness(t)
		h.withPublish()
		h.publishedRun()
		h.exportedFix()

		theirs := checksFailed
		theirs.Head = "theirs"
		h.runner.observes = []step[engine.Observation]{{res: theirs}}

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)
		assert.Equal(t, []string{"observe", "release/run"}, h.runner.trace(), "nothing is pushed over the person's commit")
		assert.Equal(t, []string{engine.StopTakenOver}, h.stopsOf())
		assert.Contains(t, h.lastStop().Explanation, "https://example.com/web/pull/7")

		run := h.lastCall("observe").runID
		assert.Equal(t, map[int64]string{run: engine.StopTakenOver}, h.spy.ended())

		// A retry gives the branch back to CAVU at the person's head, where the
		// watch sees the checks fail and a fix run starts.
		h.runner.publishes = nil
		h.fixes("mine1")
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
		h.observe(theirs)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		starts := h.runStarts()
		assert.Equal(t, ciFixFrom("theirs"), starts[len(starts)-1])
		assert.Equal(t, "theirs", h.lastCall("prepare").cfg.Repo.Head)
		assert.Equal(t, "theirs", h.lastCall("publish").pub.Prev, "the push goes on from the person's commit")
	})

	// The push of a publish whose answer was lost may have landed: the export's
	// head on the branch is CAVU's own, and the publish is repeated.
	t.Run("the open run's export, pushed by a publish that failed", func(t *testing.T) {
		h := newHarness(t)
		h.withPublish()
		h.publishedRun()
		h.exportedFix()
		h.runner.publishes = append([]step[engine.PublishResult]{{err: errors.New("connection reset by peer")}}, h.runner.publishes...)

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusBlocked, status)

		pushed := checksPending
		pushed.Head = "fix789"
		h.runner.observes = []step[engine.Observation]{{res: pushed}}
		h.clock.Advance(30 * time.Second)

		status, err = h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"observe", "publish", "release/keep", "observe", "publish", "release/run"}, h.runner.trace())
		assert.Equal(t, []string{engine.StopSandbox}, h.stopsOf())
		assert.Len(t, h.eventsOf(engine.Published), 2)
	})
}

func TestFixRunIsNotReviewed(t *testing.T) {
	tests := []struct {
		name  string
		seen  engine.Observation
		mode  string
		reply []string // the calls between the publish and the run's end
	}{
		{name: "after failed checks", seen: checksFailed, mode: "work/fix_ci"},
		{name: "after a reviewer's request", seen: askedAt("head456", "c1"), mode: "work/comments", reply: []string{"reply"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.withReview()
			h.watching()
			require.Len(t, h.runner.callsOf("review"), 1, "the first run is reviewed")
			h.fixes("fix1")

			before := len(h.runner.calls)
			assert.Equal(t, engine.StatusInReview, h.lookAt(tt.seen))
			assert.Equal(t, slices.Concat([]string{"observe", "prepare", tt.mode, "verify", "export", "observe", "publish"}, tt.reply, []string{"release/run"}),
				h.runner.trace()[before:])
		})
	}
}

// A fix run whose prepare failed ends with its environment stop; a person's
// retry starts it again with its reason, and a follow-up run with its requests.
func TestAFixRunWhosePrepareFailedIsStartedAgainAsItWas(t *testing.T) {
	setupFailed := []step[engine.PrepareResult]{{res: engine.PrepareResult{BaseCommit: "base123", FailedStep: "setup", Output: "exit 1"}}}

	t.Run("after failed checks", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.runner.prepares = setupFailed

		assert.Equal(t, engine.StatusBlocked, h.lookAt(checksFailed))
		assert.Equal(t, engine.StopEnvironment, h.lastStop().Reason)

		h.fixes("fix1")
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		before := len(h.runner.calls)
		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []string{"prepare", "work/fix_ci", "verify", "export", "observe", "publish", "release/run"}, h.runner.trace()[before:])
		assert.Equal(t, []engine.RunStartedPayload{initialRun, ciFixFrom("head456"), ciFixFrom("head456")}, h.runStarts())
	})

	t.Run("after a reviewer's request, with one that came since", func(t *testing.T) {
		h := newHarness(t)
		h.watching()
		h.runner.prepares = setupFailed

		assert.Equal(t, engine.StatusBlocked, h.lookAt(askedAt("head456", "c1")))
		assert.Equal(t, engine.StopEnvironment, h.lastStop().Reason)

		h.observe(askedAt("head456", "c1", "c2"))
		h.fixes("r1")
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))

		status, err := h.run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)
		assert.Equal(t, []engine.RunStartedPayload{
			initialRun, commentsFrom("head456", "c1"), commentsFrom("head456", "c1", "c2"),
		}, h.runStarts())

		w := h.lastCall("work").work
		assert.Equal(t, engine.ModeComments, w.Mode)
		assert.Equal(t, askedAt("head456", "c1", "c2").Comments, w.Comments)
	})
}

// Every action of a fix run works on the pull request's branch from the run's
// head, and a follow-up run's session gets the requests it was given as the
// record holds them.
func TestAFixRunWorksOnThePullRequest(t *testing.T) {
	const branch = "cavu/fix-the-login-2" // not the title's

	start := func(t *testing.T) *harness {
		t.Helper()

		h := newHarness(t)

		p := h.project()
		p.SecretVars = []string{"DB_USER"}
		require.NoError(t, h.store.UpdateProject(t.Context(), p))
		require.NoError(t, h.vault.Set(t.Context(), engine.ProjectSecretName("web", "DB_USER"), "postgres"))

		instructions := filepath.Join(t.TempDir(), "AGENTS.md")
		writeFile(t, instructions, "Work in small commits.\n", 0o644)
		h.set("instructions_path", instructions)

		pub := publishedOK
		pub.Branch = branch
		h.runner.publishes = []step[engine.PublishResult]{{res: pub}}
		h.watching()

		return h
	}

	fixes := func(h *harness, heads ...string) {
		h.fixes(heads...)

		for i := range h.runner.publishes {
			h.runner.publishes[i].res.Branch = branch
		}
	}

	// onThePullRequest checks every call of the newest run but its releases, the
	// methods of which are want. The look before the push and the reply are built
	// as the watch's look is.
	onThePullRequest := func(t *testing.T, h *harness, head string, want ...string) {
		t.Helper()

		run := h.lastCall("prepare").runID

		var methods []string

		for _, c := range h.runner.calls {
			if c.runID != run || c.method == "release" {
				continue
			}

			if !slices.Contains(methods, c.method) {
				methods = append(methods, c.method)
			}

			assert.Equal(t, branch, c.cfg.Repo.Branch, "%s", c)

			if c.method == "observe" || c.method == "reply" {
				assert.Zero(t, c.cfg.Agent.Instance, "%s reads no instance files", c)
			} else {
				assert.Equal(t, head, c.cfg.Repo.Head, "%s", c)
			}
		}

		assert.Equal(t, want, methods)
	}

	fixCalls := []string{"prepare", "work", "verify", "export", "observe", "publish"}

	t.Run("after failed checks", func(t *testing.T) {
		h := start(t)
		baseline := h.project().Baseline
		h.clock.Advance(time.Hour)

		h.runner.prepares = []step[engine.PrepareResult]{{res: engine.PrepareResult{BaseCommit: "base123", Tools: []string{"go"}, Baseline: "skipped"}}}
		fixes(h, "fix1")

		assert.Equal(t, engine.StatusInReview, h.lookAt(checksFailed))
		onThePullRequest(t, h, "head456", fixCalls...)

		w := h.lastCall("work").work
		assert.Equal(t, engine.ModeFixCI, w.Mode)
		assert.True(t, w.Resume)
		assert.Equal(t, h.runner.callsOf("work")[0].work.SessionID, w.SessionID, "the task's session")

		// A prepare at a head runs no baseline: that is no failure, and nothing is stored.
		assert.Empty(t, h.stopsOf())
		assert.Equal(t, baseline, h.project().Baseline)

		prepared := h.eventsOf(engine.Prepared)
		assert.Equal(t, "skipped", payloadOf[engine.PreparedPayload](t, prepared[len(prepared)-1]).Baseline)
	})

	t.Run("after a reviewer's requests", func(t *testing.T) {
		h := start(t)
		fixes(h, "r1", "r2")

		answered := askedAt("head456", "c0")
		h.lookAt(answered)

		asked := askedAt("r1", "c0", "c1", "c2")
		asked.Comments[1].Body = "Connect as postgres here."
		assert.Equal(t, engine.StatusInReview, h.lookAt(asked))
		onThePullRequest(t, h, "r1", append(fixCalls, "reply")...)

		seen := h.eventsOf(engine.PRObserved)
		recorded := payloadOf[engine.Observation](t, seen[len(seen)-1]).Comments

		w := h.lastCall("work").work
		assert.Equal(t, engine.ModeComments, w.Mode)
		assert.Equal(t, recorded[1:], w.Comments, "exactly the requests the run was given, as the record holds them")
		assert.NotContains(t, w.Comments[0].Body, "postgres")
	})
}

// A look before a publish that fails is a fault like the publish's own: the
// run is kept, nothing is pushed, and the look and the publish are retried.
func TestAFailedLookBeforeAPublishIsRetried(t *testing.T) {
	h := newHarness(t)
	h.withPublish()
	h.publishedRun()
	h.exportedFix()
	h.runner.observes = []step[engine.Observation]{{err: errors.New("GitHub answered 502 Bad Gateway")}}

	status, err := h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusBlocked, status)
	assert.Equal(t, []string{"observe", "release/keep"}, h.runner.trace())
	assert.Equal(t, engine.StopSandbox, h.lastStop().Reason)
	assert.Contains(t, h.lastStop().Explanation, "502")
	assert.Empty(t, h.spy.ended(), "the run stays open")

	h.clock.Advance(30 * time.Second)

	status, err = h.run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, engine.StatusInReview, status)
	assert.Equal(t, []string{"observe", "release/keep", "observe", "publish", "release/run"}, h.runner.trace())
	assert.Len(t, h.eventsOf(engine.Published), 2)
}
