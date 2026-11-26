package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readerThen calls before on its first read, then reads r.
type readerThen struct {
	before func()
	r      io.Reader
}

func (r *readerThen) Read(p []byte) (int, error) {
	if r.before != nil {
		r.before()
		r.before = nil
	}

	return r.r.Read(p)
}

// notRead fails the upload of whoever reads it.
var notRead = iotest.ErrReader(errors.New("the image was read"))

func (h *harness) images(taskID int64) []store.Image {
	h.t.Helper()

	imgs, err := h.store.TaskImages(h.t.Context(), taskID)
	require.NoError(h.t, err)

	return imgs
}

// keep stores what r holds as an image of the task past the edit rule, within most.
func (h *harness) keep(taskID int64, name string, r io.Reader, most int64) store.Image {
	h.t.Helper()

	u, err := h.store.ReceiveImage(h.t.Context(), store.Image{TaskID: taskID, Name: name, Type: "image/png"}, r, most)
	require.NoError(h.t, err)

	defer u.Discard()

	img, err := h.store.KeepImage(h.t.Context(), u, -1)
	require.NoError(h.t, err)

	return img
}

// putImage stores an image on the task past the edit rule.
func (h *harness) putImage(taskID int64, name string) store.Image {
	h.t.Helper()

	return h.keep(taskID, name, strings.NewReader(name), engine.TaskImagesMaxBytes)
}

// editRefusal is the refusal EditTask gives the task.
func (h *harness) editRefusal(taskID int64) string {
	h.t.Helper()

	err := engine.EditTask(h.t.Context(), h.store, store.Task{ID: taskID, Title: "Renamed"}, nil, nil)
	require.ErrorIs(h.t, err, engine.ErrRefused)

	return err.Error()
}

// removeImages removes the task's images ids through s, in an edit that keeps
// its title, body and dependencies.
func (h *harness) removeImages(s engine.Store, taskID int64, ids ...int64) error {
	h.t.Helper()

	task, err := h.store.Task(h.t.Context(), taskID)
	require.NoError(h.t, err)

	return engine.EditTask(h.t.Context(), s, task, h.deps(taskID), ids)
}

func (h *harness) startRun() {
	h.add(0, engine.RunStarted, "engine", `{"reason":"initial"}`)
}

func (h *harness) cancelledDraft() int64 {
	h.t.Helper()

	d := h.draft()
	require.NoError(h.t, h.loop.Cancel(h.t.Context(), d.ID))

	return d.ID
}

func TestAddImageRefusedOnceStarted(t *testing.T) {
	tests := map[string]func(h *harness) (int64, io.Reader){
		"a started task with no run, before the image is read": func(h *harness) (int64, io.Reader) {
			return h.task.ID, notRead
		},
		"a task whose run started, before the image is read": func(h *harness) (int64, io.Reader) {
			h.startRun()

			return h.task.ID, notRead
		},
		"a cancelled draft, before the image is read": func(h *harness) (int64, io.Reader) {
			return h.cancelledDraft(), notRead
		},
		"a draft started during the upload": func(h *harness) (int64, io.Reader) {
			d := h.draft()

			return d.ID, &readerThen{before: func() { h.startTask(d.ID) }, r: strings.NewReader("shot")}
		},
	}

	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			id, r := setup(h)

			_, err := engine.AddImage(t.Context(), h.store, store.Image{TaskID: id, Name: "shot.png", Type: "image/png"}, r)
			require.ErrorIs(t, err, engine.ErrRefused)
			assert.Equal(t, h.editRefusal(id), err.Error())
			assert.Empty(t, h.images(id), "nothing is kept")
			assert.Empty(t, h.files(), "nothing of the upload is left")
		})
	}
}

func TestRemoveImagesRefusedOnceStarted(t *testing.T) {
	tests := map[string]func(h *harness) int64{
		"a started task with no run": func(h *harness) int64 { return h.task.ID },
		"a task whose run started": func(h *harness) int64 {
			h.startRun()

			return h.task.ID
		},
		"a cancelled draft": (*harness).cancelledDraft,
	}

	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			id := setup(h)
			img := h.putImage(id, "shot.png")

			err := h.removeImages(h.store, id, img.ID)
			require.ErrorIs(t, err, engine.ErrRefused)
			assert.Equal(t, h.editRefusal(id), err.Error())
			assert.Equal(t, []store.Image{img}, h.images(id), "nothing is removed")
		})
	}

	t.Run("a draft's image is removed", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()
		one, two := h.putImage(d.ID, "one.png"), h.putImage(d.ID, "two.png")

		require.NoError(t, h.removeImages(h.store, d.ID, one.ID))
		assert.Equal(t, []store.Image{two}, h.images(d.ID))
	})
}

// files lists what the store's images directory holds.
func (h *harness) files() []string {
	h.t.Helper()

	entries, err := os.ReadDir(h.imagesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	require.NoError(h.t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	return names
}

// aroundWrite is a store that calls before just before it first keeps an
// image's row or writes an edit, and after just after, and counts the writes
// it was asked for.
type aroundWrite struct {
	engine.Store

	before, after func()
	writes        int
}

func (s *aroundWrite) KeepImage(ctx context.Context, u *store.Upload, expectLast int64) (store.Image, error) {
	s.writes++

	callOnce(&s.before)
	defer callOnce(&s.after)

	return s.Store.KeepImage(ctx, u, expectLast)
}

func (s *aroundWrite) EditTask(ctx context.Context, t store.Task, dependsOn, removeImages []int64, expectLast int64) error {
	s.writes++

	callOnce(&s.before)
	defer callOnce(&s.after)

	return s.Store.EditTask(ctx, t, dependsOn, removeImages, expectLast)
}

func callOnce(f *func()) {
	if g := *f; g != nil {
		*f = nil

		g()
	}
}

// startable is a draft whose project has a baseline, so a person can start it.
func (h *harness) startable() int64 {
	h.t.Helper()
	h.withBaseline()

	return h.draft().ID
}

// startingAround is the store that has a person start the draft just before
// it writes an image's row or an edit when before is set, else just after.
// session runs the draft to review and gives the config of its one session.
func (h *harness) startingAround(id int64, before bool) (s *aroundWrite, session func() engine.RunConfig) {
	started := false
	start := func() {
		require.NoError(h.t, h.loop.Start(h.t.Context(), id))

		started = true
	}

	around := &aroundWrite{Store: h.store}
	if before {
		around.before = start
	} else {
		around.after = start
	}

	return around, func() engine.RunConfig {
		h.t.Helper()
		require.True(h.t, started, "the draft was started")

		status, err := h.loop.RunTask(h.t.Context(), id)
		require.NoError(h.t, err)
		require.Equal(h.t, engine.StatusInReview, status)

		works := h.runner.callsOf("work")
		require.Len(h.t, works, 1)

		return works[0].cfg
	}
}

// An upload or a removal as a draft is started comes before the start, and
// its run has what it leaves, or is refused and leaves the run what was there.
func TestImageWritesAsTheDraftIsStarted(t *testing.T) {
	shot := engine.TaskImage{Name: "shot.png", Type: "image/png", Data: []byte("shot.png")}
	upload := func(s engine.Store, taskID int64) (store.Image, error) {
		return engine.AddImage(t.Context(), s, store.Image{TaskID: taskID, Name: "shot.png", Type: "image/png"}, strings.NewReader("shot.png"))
	}

	t.Run("an upload the start comes before is refused and never reaches the run", func(t *testing.T) {
		h := newHarness(t)
		id := h.startable()
		s, session := h.startingAround(id, true)

		_, err := upload(s, id)
		require.ErrorIs(t, err, engine.ErrRefused)
		assert.Equal(t, h.editRefusal(id), err.Error())
		assert.Equal(t, 1, s.writes, "the write that lost to the start is not made again")
		assert.Empty(t, h.images(id), "nothing is kept")
		assert.Empty(t, h.files(), "nothing of the upload is left")
		assert.Empty(t, session().Task.Images)
	})

	t.Run("an upload kept before the start reaches the run", func(t *testing.T) {
		h := newHarness(t)
		id := h.startable()
		s, session := h.startingAround(id, false)

		img, err := upload(s, id)
		require.NoError(t, err)
		assert.Equal(t, []store.Image{img}, h.images(id))
		assert.Equal(t, []engine.TaskImage{shot}, session().Task.Images)
	})

	t.Run("a removal the start comes before is refused and leaves the run its image", func(t *testing.T) {
		h := newHarness(t)
		id := h.startable()
		img := h.putImage(id, "shot.png")
		s, session := h.startingAround(id, true)

		err := h.removeImages(s, id, img.ID)
		require.ErrorIs(t, err, engine.ErrRefused)
		assert.Equal(t, h.editRefusal(id), err.Error())
		assert.Equal(t, []store.Image{img}, h.images(id), "nothing is removed")
		assert.Equal(t, []engine.TaskImage{shot}, session().Task.Images)
	})

	t.Run("a removal made before the start is not in the run", func(t *testing.T) {
		h := newHarness(t)
		id := h.startable()
		img := h.putImage(id, "shot.png")
		s, session := h.startingAround(id, false)

		require.NoError(t, h.removeImages(s, id, img.ID))
		assert.Empty(t, h.images(id))
		assert.Empty(t, h.files(), "the image's contents are gone")
		assert.Empty(t, session().Task.Images)
	})
}

// An edit as a draft is started reaches its run whole, or is refused and
// leaves the run the task as it was.
func TestEditAsTheDraftIsStarted(t *testing.T) {
	kept := engine.TaskImage{Name: "kept.png", Type: "image/png", Data: []byte("kept.png")}
	gone := engine.TaskImage{Name: "gone.png", Type: "image/png", Data: []byte("gone.png")}

	// withImages is a draft, ready to start, as stored, with its images kept.png and gone.png.
	withImages := func(t *testing.T) (*harness, store.Task, []store.Image) {
		h := newHarness(t)
		id := h.startable()
		imgs := []store.Image{h.putImage(id, kept.Name), h.putImage(id, gone.Name)}

		was, err := h.store.Task(t.Context(), id)
		require.NoError(t, err)

		return h, was, imgs
	}

	edit := func(s engine.Store, was store.Task, gone store.Image) error {
		return engine.EditTask(t.Context(), s, store.Task{ID: was.ID, Title: "Renamed", Body: "A new body."}, nil, []int64{gone.ID})
	}

	t.Run("an edit the start comes before is refused and leaves the run the task", func(t *testing.T) {
		h, was, imgs := withImages(t)
		s, session := h.startingAround(was.ID, true)

		err := edit(s, was, imgs[1])
		require.ErrorIs(t, err, engine.ErrRefused)
		assert.Equal(t, h.editRefusal(was.ID), err.Error())
		assert.Equal(t, 1, s.writes, "the write that lost to the start is not made again")

		got, err := h.store.Task(t.Context(), was.ID)
		require.NoError(t, err)
		assert.Equal(t, was, got, "nothing is written")
		assert.Equal(t, imgs, h.images(was.ID), "nothing is removed")

		cfg := session()
		assert.Equal(t, was.Title, cfg.Task.Title)
		assert.Equal(t, was.Body, cfg.Task.Body)
		assert.Equal(t, []engine.TaskImage{kept, gone}, cfg.Task.Images)
	})

	t.Run("an edit made before the start reaches the run whole", func(t *testing.T) {
		h, was, imgs := withImages(t)
		s, session := h.startingAround(was.ID, false)

		require.NoError(t, edit(s, was, imgs[1]))
		assert.Equal(t, imgs[:1], h.images(was.ID))
		assert.Equal(t, []string{imgs[0].SHA256}, h.files(), "the removed image's contents are gone")

		cfg := session()
		assert.Equal(t, "Renamed", cfg.Task.Title)
		assert.Equal(t, "A new body.", cfg.Task.Body)
		assert.Equal(t, []engine.TaskImage{kept}, cfg.Task.Images)
	})
}

// contents reads the contents of the task's image id.
func (h *harness) contents(taskID, id int64) []byte {
	h.t.Helper()

	_, f, err := h.store.OpenImage(h.t.Context(), taskID, id)
	require.NoError(h.t, err)

	defer f.Close()

	data, err := io.ReadAll(f)
	require.NoError(h.t, err)

	return data
}

// A write that loses to an event the task stays a draft after is made again
// at that event. A progress event is one: Reduce reads none.
func TestImageWritesRetryOnceTheTaskStaysADraft(t *testing.T) {
	// eventAround is a draft and the store that records such an event on it just before the first write.
	eventAround := func(t *testing.T) (*harness, int64, *aroundWrite) {
		h := newHarness(t)
		d := h.draft()

		return h, d.ID, &aroundWrite{Store: h.store, before: func() { h.addTo(d.ID, now, engine.AgentText, struct{}{}) }}
	}

	t.Run("an upload is kept once, with its contents", func(t *testing.T) {
		h, id, s := eventAround(t)

		img, err := engine.AddImage(t.Context(), s, store.Image{TaskID: id, Name: "shot.png", Type: "image/png"}, strings.NewReader("shot"))
		require.NoError(t, err)
		assert.Equal(t, 2, s.writes, "the first write lost to the event")
		assert.Equal(t, engine.StatusDraft, h.viewOf(id, nil).Status)
		assert.Equal(t, []store.Image{img}, h.images(id))
		assert.Equal(t, []byte("shot"), h.contents(id, img.ID))
		assert.Equal(t, []string{img.SHA256}, h.files(), "no temporary file")
	})

	t.Run("an edit with a removal is made whole", func(t *testing.T) {
		h, id, s := eventAround(t)
		img := h.putImage(id, "shot.png")

		require.NoError(t, engine.EditTask(t.Context(), s, store.Task{ID: id, Title: "Renamed", Body: "A new body."}, nil, []int64{img.ID}))
		assert.Equal(t, 2, s.writes, "the first write lost to the event")
		assert.Equal(t, engine.StatusDraft, h.viewOf(id, nil).Status)

		got, err := h.store.Task(t.Context(), id)
		require.NoError(t, err)
		assert.Equal(t, "Renamed", got.Title)
		assert.Equal(t, "A new body.", got.Body)
		assert.Empty(t, h.images(id))
		assert.Empty(t, h.files(), "the image's contents are gone")
	})
}

func TestViewCarriesImages(t *testing.T) {
	h := newHarness(t)
	id := h.draft().ID

	b, err := json.Marshal(h.viewOf(id, nil))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, []any{}, got["images"], "an empty list, never null")

	add := func(name, typ, data string) store.Image {
		img, err := engine.AddImage(t.Context(), h.store, store.Image{TaskID: id, Name: name, Type: typ}, strings.NewReader(data))
		require.NoError(t, err)

		return img
	}

	one := add("save-button.png", "image/png", "first")
	two := add("dialog.webp", "image/webp", "second")

	assert.Equal(t, []engine.Image{
		{ID: one.ID, Name: "save-button.png", Type: "image/png", Size: 5},
		{ID: two.ID, Name: "dialog.webp", Type: "image/webp", Size: 6},
	}, h.viewOf(id, nil).Images, "in the order added")
}

// putBytes stores n bytes as an image of the task past the edit rule and the
// cap, as an image kept before the cap was.
func (h *harness) putBytes(taskID, n int64) store.Image {
	h.t.Helper()

	return h.keep(taskID, "shot.png", bytes.NewReader(make([]byte, n)), math.MaxInt64)
}

func TestAddImageKeepsTheTaskWithinTheCap(t *testing.T) {
	h := newHarness(t)
	d := h.draft()
	h.putBytes(d.ID, engine.TaskImagesMaxBytes-1)

	add := func(data string) error {
		_, err := engine.AddImage(t.Context(), h.store, store.Image{TaskID: d.ID, Name: "shot.png", Type: "image/png"}, strings.NewReader(data))

		return err
	}

	require.ErrorIs(t, add("xy"), store.ErrImagesFull)
	require.NoError(t, add("x"), "the cap itself is taken")
	require.ErrorIs(t, add("x"), store.ErrImagesFull)
}

func TestStartRefusesADraftPastTheImageCap(t *testing.T) {
	h := newHarness(t)
	h.withBaseline()
	d := h.draft()
	h.putBytes(d.ID, engine.TaskImagesMaxBytes/2)
	h.putBytes(d.ID, engine.TaskImagesMaxBytes/2)
	extra := h.putBytes(d.ID, 1)

	require.ErrorIs(t, h.loop.Start(t.Context(), d.ID), engine.ErrRefused)
	assert.Equal(t, engine.StatusDraft, h.viewOf(d.ID, nil).Status)

	require.NoError(t, h.removeImages(h.store, d.ID, extra.ID))
	require.NoError(t, h.loop.Start(t.Context(), d.ID), "images that hold the cap itself start")
	assert.Equal(t, engine.StatusQueued, h.viewOf(d.ID, nil).Status)
}

// stoppedForImages runs the task and wants it stopped as images, for a person
// to clear, with no image's contents read.
func (h *harness) stoppedForImages() {
	h.t.Helper()

	status, err := h.run(h.t.Context())
	require.NoError(h.t, err)
	require.Equal(h.t, engine.StatusBlocked, status)

	v := h.viewOf(h.task.ID, nil)
	require.NotNil(h.t, v.Stop)
	assert.Equal(h.t, engine.StopImages, v.Stop.Reason)
	assert.Equal(h.t, engine.ClearRetry, v.Stop.Action)
	assert.True(h.t, v.NeedsYou)
	assert.Zero(h.t, h.spy.opened(), "no image's contents are read")
}

// A task whose images hold more than the cap, as only one that started before
// the cap can, runs no prepare and no session.
func TestImagesPastTheCapStopTheTask(t *testing.T) {
	t.Run("before a new run's prepare, and again after a retry", func(t *testing.T) {
		h := newHarness(t)
		h.putBytes(h.task.ID, engine.TaskImagesMaxBytes/2)
		h.putBytes(h.task.ID, engine.TaskImagesMaxBytes/2+1)

		h.stoppedForImages()
		require.NoError(t, h.loop.Retry(t.Context(), h.task.ID))
		h.stoppedForImages()

		assert.Empty(t, h.runner.callsOf("prepare"))

		require.Len(t, h.spy.ended(), 2, "each new run is abandoned")

		for id, outcome := range h.spy.ended() {
			assert.Equal(t, engine.RunAbandoned, outcome, "run %d", id)
		}
	})

	t.Run("before a session of an open run, until a person restores the task and removes images", func(t *testing.T) {
		h := newHarness(t)
		h.withBaseline()
		ctx, id := t.Context(), h.task.ID
		kept := h.putImage(id, "kept.png")

		var over []store.Image

		h.runner.onCall = func(c call) {
			if c.method == "prepare" && over == nil {
				over = []store.Image{h.putBytes(id, engine.TaskImagesMaxBytes/2), h.putBytes(id, engine.TaskImagesMaxBytes/2)}
			}
		}

		h.stoppedForImages()
		assert.Empty(t, h.runner.callsOf("work"))
		assert.Equal(t, []string{"prepare", "release/keep"}, h.runner.callsFor(id), "the run keeps its volumes")

		require.NoError(t, h.loop.Cancel(ctx, id))
		status, err := h.run(ctx)
		require.NoError(t, err)
		require.Equal(t, engine.StatusCancelled, status)
		require.NoError(t, h.loop.Restore(ctx, id))
		require.NoError(t, h.removeImages(h.store, id, over[0].ID))
		require.NoError(t, h.loop.Start(ctx, id))

		status, err = h.run(ctx)
		require.NoError(t, err)
		assert.Equal(t, engine.StatusInReview, status)

		works := h.runner.callsOf("work")
		require.Len(t, works, 1)
		assert.Equal(t, []int64{kept.Size, over[1].Size}, imageSizes(works[0].cfg.Task.Images))
	})
}

func imageSizes(imgs []engine.TaskImage) []int64 {
	sizes := make([]int64, len(imgs))
	for i, img := range imgs {
		sizes[i] = int64(len(img.Data))
	}

	return sizes
}
