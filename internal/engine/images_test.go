package engine_test

import (
	"encoding/json"
	"errors"
	"io"
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

	img, err := h.store.PutImage(h.t.Context(), store.Image{TaskID: taskID, Name: name, Type: "image/png"}, strings.NewReader(name))
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
