package engine

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/mhersson/cavu/internal/store"
)

// ImageMaxBytes is the most one image of a task may hold.
const ImageMaxBytes int64 = 10 << 20

// TaskImagesMaxBytes is the most a task's images may hold together. It bounds
// what a session's config carries, which holds every image's bytes.
const TaskImagesMaxBytes int64 = 32 << 20

// ImageTypes are the media types an image of a task may have.
var ImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// AddImage keeps what r holds as an image of img.TaskID, refused as EditTask
// is. The rule is checked before r is read, and again once it is, in a write
// that loses to any event recorded since: a task that is started has every
// image this returns and none it refuses. An image that would take the task's
// images past TaskImagesMaxBytes together is store.ErrImagesFull.
func AddImage(ctx context.Context, s Store, img store.Image, r io.Reader) (store.Image, error) {
	if err := editable(ctx, s, img.TaskID); err != nil {
		return store.Image{}, err
	}

	u, err := s.ReceiveImage(ctx, img, r, TaskImagesMaxBytes)
	if err != nil {
		return store.Image{}, fmt.Errorf("task %d: %w", img.TaskID, err)
	}
	defer u.Discard()

	var kept store.Image

	err = whileEditable(ctx, s, img.TaskID, func(last int64) (err error) {
		kept, err = s.KeepImage(ctx, u, last)

		return err
	})
	if err != nil {
		return store.Image{}, err
	}

	return kept, nil
}

// imagesOver is what the task's images hold together, as text in MiB, when
// that is more than TaskImagesMaxBytes, else "".
func imagesOver(ctx context.Context, s Store, taskID int64) (string, error) {
	imgs, err := s.TaskImages(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("task %d: %w", taskID, err)
	}

	return overOf(imgs), nil
}

// overOf is imagesOver's text for imgs.
func overOf(imgs []store.Image) string {
	var held int64
	for _, img := range imgs {
		held += img.Size
	}

	if held <= TaskImagesMaxBytes {
		return ""
	}

	return strconv.FormatFloat(float64(held)/(1<<20), 'f', 1, 64) + " MiB"
}

// taskImages reads the task's images in the order added, each exactly as
// large as recorded, or returns imagesFull before it reads any when they hold
// more than TaskImagesMaxBytes together.
func (l *Loop) taskImages(ctx context.Context, taskID int64) ([]TaskImage, error) {
	imgs, err := l.Store.TaskImages(ctx, taskID)
	if err != nil {
		return nil, err
	}

	if held := overOf(imgs); held != "" {
		return nil, imagesFull(held)
	}

	var out []TaskImage

	for _, img := range imgs {
		_, f, err := l.Store.OpenImage(ctx, taskID, img.ID)
		if err != nil {
			return nil, err
		}

		data := make([]byte, img.Size)
		_, err = io.ReadFull(f, data)
		_ = f.Close()

		if err != nil {
			return nil, fmt.Errorf("read image %d of task %d: %w", img.ID, taskID, err)
		}

		out = append(out, TaskImage{Name: img.Name, Type: img.Type, Data: data})
	}

	return out, nil
}

// imagesFull is what the images hold, as imagesOver gives it, of a task that
// may run no prepare or session while they hold more than TaskImagesMaxBytes.
type imagesFull string

func (e imagesFull) Error() string { return "the task's images " + e.hold() }

func (e imagesFull) hold() string {
	return fmt.Sprintf("hold %s, more than the %d MiB a task's images may hold together", string(e), TaskImagesMaxBytes>>20)
}
