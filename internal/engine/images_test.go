package engine_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
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

// putImage stores an image on the task past the edit rule.
func (h *harness) putImage(taskID int64, name string) store.Image {
	h.t.Helper()

	img, err := h.store.PutImage(h.t.Context(), store.Image{TaskID: taskID, Name: name, Type: "image/png"}, strings.NewReader(name), engine.TaskImagesMaxBytes)
	require.NoError(h.t, err)

	return img
}

// editRefusal is the refusal EditTask gives the task.
func (h *harness) editRefusal(taskID int64) string {
	h.t.Helper()

	err := engine.EditTask(h.t.Context(), h.store, store.Task{ID: taskID, Title: "Renamed"}, nil)
	require.ErrorIs(h.t, err, engine.ErrRefused)

	return err.Error()
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

func TestAddImageRefusedOnceARunStarted(t *testing.T) {
	tests := map[string]func(h *harness) (int64, io.Reader){
		"a task whose run started, before the image is read": func(h *harness) (int64, io.Reader) {
			h.startRun()

			return h.task.ID, notRead
		},
		"a cancelled draft, before the image is read": func(h *harness) (int64, io.Reader) {
			return h.cancelledDraft(), notRead
		},
		"a task whose run starts during the upload": func(h *harness) (int64, io.Reader) {
			return h.task.ID, &readerThen{before: h.startRun, r: strings.NewReader("shot")}
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
		})
	}
}

func TestRemoveImagesRefusedOnceARunStarted(t *testing.T) {
	tests := map[string]func(h *harness) int64{
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

			err := engine.RemoveImages(t.Context(), h.store, id, []int64{img.ID})
			require.ErrorIs(t, err, engine.ErrRefused)
			assert.Equal(t, h.editRefusal(id), err.Error())
			assert.Equal(t, []store.Image{img}, h.images(id), "nothing is removed")
		})
	}

	t.Run("a draft's image is removed", func(t *testing.T) {
		h := newHarness(t)
		d := h.draft()
		one, two := h.putImage(d.ID, "one.png"), h.putImage(d.ID, "two.png")

		require.NoError(t, engine.RemoveImages(t.Context(), h.store, d.ID, []int64{one.ID}))
		assert.Equal(t, []store.Image{two}, h.images(d.ID))
	})
}

func TestViewCarriesImages(t *testing.T) {
	h := newHarness(t)
	id := h.task.ID

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

	img, err := h.store.PutImage(h.t.Context(), store.Image{TaskID: taskID, Name: "shot.png", Type: "image/png"},
		bytes.NewReader(make([]byte, n)), math.MaxInt64)
	require.NoError(h.t, err)

	return img
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

	require.NoError(t, engine.RemoveImages(t.Context(), h.store, d.ID, []int64{extra.ID}))
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
		require.NoError(t, engine.RemoveImages(ctx, h.store, id, []int64{over[0].ID}))
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
