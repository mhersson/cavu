package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

func putImage(t *testing.T, s *Store, taskID int64, name string, data []byte) Image {
	t.Helper()

	img, err := s.PutImage(t.Context(), Image{TaskID: taskID, Name: name, Type: "image/png"}, bytes.NewReader(data))
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

func TestPutImage(t *testing.T) {
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

	require.NoError(t, s.RemoveImages(t.Context(), a.ID, []int64{ia.ID}))
	assert.Empty(t, taskImages(t, s, a.ID))
	assert.Equal(t, []Image{ib}, taskImages(t, s, b.ID))
	assert.Equal(t, data, readImage(t, s, b.ID, ib.ID), "the other task still has its contents")
	assert.Equal(t, []string{ia.SHA256}, imageFiles(t, dir))

	require.NoError(t, s.RemoveImages(t.Context(), b.ID, []int64{ib.ID}))
	assert.Empty(t, taskImages(t, s, b.ID))
	assert.Empty(t, imageFiles(t, dir), "the last row's removal removes the contents")
}

func TestPutImageLeavesNothingOnFailure(t *testing.T) {
	t.Parallel()

	t.Run("a reader that fails half way", func(t *testing.T) {
		t.Parallel()

		s, dir := openWithImages(t)
		task := seedTask(t, s)
		lost := errors.New("connection lost")

		_, err := s.PutImage(t.Context(), Image{TaskID: task.ID, Name: "a.png", Type: "image/png"},
			io.MultiReader(strings.NewReader("half an image"), iotest.ErrReader(lost)))
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
			_, err := s.PutImage(t.Context(), Image{TaskID: 999, Name: "a.png", Type: "image/png"}, strings.NewReader(data))
			require.Error(t, err, data)
		}

		assert.Equal(t, []string{kept.SHA256}, imageFiles(t, dir))
		assert.Equal(t, []byte("kept"), readImage(t, s, task.ID, kept.ID))
	})
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

func TestRemoveImagesRemovesNothingWhenOneIsAbsent(t *testing.T) {
	t.Parallel()

	s, dir := openWithImages(t)
	a := seedTask(t, s)
	b := otherTask(t, s, a)
	img := putImage(t, s, a.ID, "shot.png", []byte("shot"))

	require.ErrorIs(t, s.RemoveImages(t.Context(), a.ID, []int64{img.ID, img.ID + 1}), ErrNotFound)
	require.ErrorIs(t, s.RemoveImages(t.Context(), b.ID, []int64{img.ID}), ErrNotFound, "another task's image")

	assert.Equal(t, []Image{img}, taskImages(t, s, a.ID))
	assert.Equal(t, []string{img.SHA256}, imageFiles(t, dir))
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

	require.NoError(t, s.RemoveImages(t.Context(), src.ID, []int64{one.ID, two.ID}))
	assert.Empty(t, taskImages(t, s, src.ID))
	assert.Equal(t, []byte("one"), readImage(t, s, shadow.ID, got[0].ID), "the copy keeps the contents")
	assert.Equal(t, []byte("two"), readImage(t, s, shadow.ID, got[1].ID))
}
