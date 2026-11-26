package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openWithImages opens a store in a directory of the test's own and gives the
// directory its images are kept in, beside the database.
func openWithImages(t *testing.T) (*Store, string) {
	t.Helper()

	dir := t.TempDir()

	s, err := Open(context.Background(), filepath.Join(dir, "cavu.db"))
	require.NoError(t, err)

	t.Cleanup(func() { _ = s.Close() })

	return s, filepath.Join(dir, "images")
}

// imageFiles lists every name in dir, none when it does not exist.
func imageFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	return names
}

// notRead fails the upload of whoever reads it.
var notRead = iotest.ErrReader(errors.New("the image was read"))

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

// keep receives what r holds and keeps it as img, whatever the task's events.
func keep(ctx context.Context, s *Store, img Image, r io.Reader, most int64) (Image, error) {
	u, err := s.ReceiveImage(ctx, img, r, most)
	if err != nil {
		return Image{}, err
	}
	defer u.Discard()

	return s.KeepImage(ctx, u, -1)
}

func putImage(t *testing.T, s *Store, taskID int64, name string, data []byte) Image {
	t.Helper()

	img, err := keep(t.Context(), s, Image{TaskID: taskID, Name: name, Type: "image/png"}, bytes.NewReader(data), math.MaxInt64)
	require.NoError(t, err)

	return img
}

func taskImages(t *testing.T, s *Store, taskID int64) []Image {
	t.Helper()

	imgs, err := s.TaskImages(t.Context(), taskID)
	require.NoError(t, err)

	return imgs
}

// readImage gives the contents of the task's image id.
func readImage(t *testing.T, s *Store, taskID, id int64) []byte {
	t.Helper()

	_, f, err := s.OpenImage(t.Context(), taskID, id)
	require.NoError(t, err)

	defer f.Close()

	b, err := io.ReadAll(f)
	require.NoError(t, err)

	return b
}

func otherTask(t *testing.T, s *Store, of Task) Task {
	t.Helper()

	task, err := s.CreateTask(t.Context(), Task{ProjectID: of.ProjectID, Title: "other"})
	require.NoError(t, err)

	return task
}

func TestKeepImage(t *testing.T) {
	t.Parallel()

	s, dir := openWithImages(t)
	task := seedTask(t, s)
	data := []byte("\x89PNG\r\n\x1a\n a screenshot of the save button")
	sum := sha256.Sum256(data)

	img := putImage(t, s, task.ID, "save-button.png", data)
	assert.NotZero(t, img.ID)
	assert.Equal(t, task.ID, img.TaskID)
	assert.Equal(t, "save-button.png", img.Name)
	assert.Equal(t, "image/png", img.Type)
	assert.Equal(t, int64(len(data)), img.Size)
	assert.Equal(t, hex.EncodeToString(sum[:]), img.SHA256)
	assert.False(t, img.CreatedAt.IsZero())

	assert.Equal(t, []Image{img}, taskImages(t, s, task.ID), "the row reads back as it was returned")
	assert.Equal(t, []string{img.SHA256}, imageFiles(t, dir), "one file, named by its contents, and no temporary file")
	assert.Equal(t, data, readImage(t, s, task.ID, img.ID))

	info, err := os.Stat(filepath.Join(dir, img.SHA256))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	info, err = os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	second := putImage(t, s, task.ID, "second.png", []byte("another"))
	assert.Equal(t, []Image{img, second}, taskImages(t, s, task.ID), "in the order added")
}

func TestImagesShareContents(t *testing.T) {
	t.Parallel()

	s, dir := openWithImages(t)
	a := seedTask(t, s)
	b := otherTask(t, s, a)
	data := []byte("the same screenshot")

	ia := putImage(t, s, a.ID, "a.png", data)
	ib := putImage(t, s, b.ID, "b.png", data)
	assert.Equal(t, ia.SHA256, ib.SHA256)
	assert.Equal(t, []string{ia.SHA256}, imageFiles(t, dir), "one file for both")

	require.NoError(t, s.EditTask(t.Context(), a, nil, []int64{ia.ID}, -1))
	assert.Empty(t, taskImages(t, s, a.ID))
	assert.Equal(t, []Image{ib}, taskImages(t, s, b.ID))
	assert.Equal(t, data, readImage(t, s, b.ID, ib.ID), "the other task still has its contents")
	assert.Equal(t, []string{ia.SHA256}, imageFiles(t, dir))

	require.NoError(t, s.EditTask(t.Context(), b, nil, []int64{ib.ID}, -1))
	assert.Empty(t, taskImages(t, s, b.ID))
	assert.Empty(t, imageFiles(t, dir), "the last row's removal removes the contents")
}

func TestKeepImageLeavesNothingOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("a reader that fails half way", func(t *testing.T) {
		t.Parallel()

		s, dir := openWithImages(t)
		task := seedTask(t, s)
		lost := errors.New("connection lost")

		_, err := keep(t.Context(), s, Image{TaskID: task.ID, Name: "a.png", Type: "image/png"},
			io.MultiReader(strings.NewReader("half an image"), iotest.ErrReader(lost)), math.MaxInt64)
		require.ErrorIs(t, err, lost)
		assert.Empty(t, taskImages(t, s, task.ID))
		assert.Empty(t, imageFiles(t, dir))
	})

	t.Run("a row that cannot be written keeps contents another row names", func(t *testing.T) {
		t.Parallel()

		s, dir := openWithImages(t)
		task := seedTask(t, s)
		kept := putImage(t, s, task.ID, "kept.png", []byte("kept"))

		for _, data := range []string{"new", "kept"} {
			_, err := keep(t.Context(), s, Image{TaskID: 999, Name: "a.png", Type: "image/png"}, strings.NewReader(data), math.MaxInt64)
			require.Error(t, err, data)
		}

		assert.Equal(t, []string{kept.SHA256}, imageFiles(t, dir))
		assert.Equal(t, []byte("kept"), readImage(t, s, task.ID, kept.ID))
	})
}

// receiveImage receives data as an upload of an image of the task.
func receiveImage(t *testing.T, s *Store, taskID int64, data string) *Upload {
	t.Helper()

	u, err := s.ReceiveImage(t.Context(), Image{TaskID: taskID, Name: "a.png", Type: "image/png"}, strings.NewReader(data), math.MaxInt64)
	require.NoError(t, err)

	return u
}

// An upload kept at an event older than the task's newest keeps nothing.
func TestKeepImageLosesToANewerEvent(t *testing.T) {
	t.Parallel()

	t.Run("and is kept at the newest", func(t *testing.T) {
		t.Parallel()

		s, dir := openWithImages(t)
		task := seedTask(t, s)
		seen := appendN(t, s, task.ID, 1)
		u := receiveImage(t, s, task.ID, "shot")
		newest := appendN(t, s, task.ID, 1)

		_, err := s.KeepImage(t.Context(), u, seen)
		require.ErrorIs(t, err, ErrConflict)
		assert.Empty(t, taskImages(t, s, task.ID))

		img, err := s.KeepImage(t.Context(), u, newest)
		require.NoError(t, err)
		u.Discard()
		assert.Equal(t, []Image{img}, taskImages(t, s, task.ID))
		assert.Equal(t, []byte("shot"), readImage(t, s, task.ID, img.ID))
		assert.Equal(t, []string{img.SHA256}, imageFiles(t, dir), "no temporary file")
	})

	for name, data := range map[string]string{"new contents": "new", "contents another image names": "shared"} {
		t.Run("and is discarded, with "+name, func(t *testing.T) {
			t.Parallel()

			s, dir := openWithImages(t)
			task := seedTask(t, s)
			other := otherTask(t, s, task)
			shared := putImage(t, s, other.ID, "shared.png", []byte("shared"))
			seen := appendN(t, s, task.ID, 1)
			u := receiveImage(t, s, task.ID, data)
			appendN(t, s, task.ID, 1)

			_, err := s.KeepImage(t.Context(), u, seen)
			require.ErrorIs(t, err, ErrConflict)
			u.Discard()

			assert.Empty(t, taskImages(t, s, task.ID))
			assert.Equal(t, []string{shared.SHA256}, imageFiles(t, dir), "only the contents a row names")
			assert.Equal(t, []byte("shared"), readImage(t, s, other.ID, shared.ID))
		})
	}
}

func TestKeepImageKeepsTheTaskWithinMost(t *testing.T) {
	t.Parallel()

	const most = 10

	put := func(s *Store, taskID int64, r io.Reader, m int64) error {
		_, err := keep(t.Context(), s, Image{TaskID: taskID, Name: "a.png", Type: "image/png"}, r, m)

		return err
	}

	t.Run("up to most together, and not a byte more", func(t *testing.T) {
		t.Parallel()

		s, dir := openWithImages(t)
		task := seedTask(t, s)
		other := otherTask(t, s, task)

		require.NoError(t, put(s, task.ID, strings.NewReader("123456"), most))
		require.NoError(t, put(s, task.ID, strings.NewReader("7890"), most), "most itself is taken")

		files := imageFiles(t, dir)
		require.ErrorIs(t, put(s, task.ID, strings.NewReader("x"), most), ErrImagesFull)
		assert.Len(t, taskImages(t, s, task.ID), 2)
		assert.Equal(t, files, imageFiles(t, dir), "the refused image left nothing behind")

		require.NoError(t, put(s, other.ID, strings.NewReader("1234567890"), most), "each task has its own room")
	})

	t.Run("an image past the room is read only one byte past it", func(t *testing.T) {
		t.Parallel()

		s, _ := openWithImages(t)
		task := seedTask(t, s)
		require.NoError(t, put(s, task.ID, strings.NewReader("123456"), most))

		r := io.MultiReader(strings.NewReader("abcde"), notRead)
		require.ErrorIs(t, put(s, task.ID, r, most), ErrImagesFull)
	})

	t.Run("a task that holds more than most already is refused before a byte is read", func(t *testing.T) {
		t.Parallel()

		s, _ := openWithImages(t)
		task := seedTask(t, s)
		require.NoError(t, put(s, task.ID, strings.NewReader("1234567890ab"), math.MaxInt64))

		require.ErrorIs(t, put(s, task.ID, notRead, most), ErrImagesFull)
	})
}

// Uploads that each fit alone but not together all pass the first check of the
// room before any is kept; only those that fit are kept.
func TestConcurrentUploadsKeepTheTaskWithinMost(t *testing.T) {
	t.Parallel()

	const (
		uploads = 5
		size    = 3
		most    = 10
	)

	s, dir := openWithImages(t)
	task := seedTask(t, s)

	var all sync.WaitGroup

	all.Add(uploads)

	errs := make([]error, uploads)

	var wg sync.WaitGroup

	for i := range uploads {
		wg.Go(func() {
			r := &readerThen{before: func() { all.Done(); all.Wait() }, r: strings.NewReader(strings.Repeat(string(rune('a'+i)), size))}
			_, errs[i] = keep(t.Context(), s, Image{TaskID: task.ID, Name: "a.png", Type: "image/png"}, r, most)
		})
	}

	wg.Wait()

	var kept int

	for _, err := range errs {
		if err == nil {
			kept++

			continue
		}

		require.ErrorIs(t, err, ErrImagesFull)
	}

	assert.Equal(t, most/size, kept)

	var held int64
	for _, img := range taskImages(t, s, task.ID) {
		held += img.Size
	}

	assert.LessOrEqual(t, held, int64(most))
	assert.Len(t, imageFiles(t, dir), kept, "a refused upload's contents are not kept")
}

func TestOpenImage(t *testing.T) {
	t.Parallel()

	s, _ := openWithImages(t)
	a := seedTask(t, s)
	b := otherTask(t, s, a)
	img := putImage(t, s, a.ID, "shot.png", []byte("shot"))

	got, f, err := s.OpenImage(t.Context(), a.ID, img.ID)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.Equal(t, img, got)

	for name, ids := range map[string][2]int64{
		"another task's image": {b.ID, img.ID},
		"an image not there":   {a.ID, img.ID + 1},
	} {
		_, _, err := s.OpenImage(t.Context(), ids[0], ids[1])
		require.ErrorIs(t, err, ErrNotFound, name)
	}
}

func TestCopyImages(t *testing.T) {
	t.Parallel()

	s, dir := openWithImages(t)
	src := seedTask(t, s)
	shadow := otherTask(t, s, src)
	one := putImage(t, s, src.ID, "one.png", []byte("one"))
	two := putImage(t, s, src.ID, "two.png", []byte("two"))

	require.NoError(t, s.CopyImages(t.Context(), src.ID, shadow.ID))

	got := taskImages(t, s, shadow.ID)
	require.Len(t, got, 2)

	for i, want := range []Image{one, two} {
		assert.NotEqual(t, want.ID, got[i].ID, "a row of its own")
		assert.Equal(t, shadow.ID, got[i].TaskID)
		assert.Equal(t, want.Name, got[i].Name)
		assert.Equal(t, want.Type, got[i].Type)
		assert.Equal(t, want.Size, got[i].Size)
		assert.Equal(t, want.SHA256, got[i].SHA256)
	}

	assert.Len(t, imageFiles(t, dir), 2, "the contents are shared")

	require.NoError(t, s.EditTask(t.Context(), src, nil, []int64{one.ID, two.ID}, -1))
	assert.Empty(t, taskImages(t, s, src.ID))
	assert.Equal(t, []byte("one"), readImage(t, s, shadow.ID, got[0].ID), "the copy keeps the contents")
	assert.Equal(t, []byte("two"), readImage(t, s, shadow.ID, got[1].ID))
}
