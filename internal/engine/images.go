package engine

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/mhersson/cavu/internal/store"
)

// ImageMaxBytes is the most one image of a task may hold.
const ImageMaxBytes int64 = 10 << 20

// ImageTypes are the media types an image of a task may have.
var ImageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// AddImage keeps what r holds as an image of img.TaskID, refused as EditTask
// is. The rule is checked before r is read and again once the image is kept,
// so a run that started meanwhile never lacks an image this returns.
func AddImage(ctx context.Context, s Store, img store.Image, r io.Reader) (store.Image, error) {
	if _, err := editable(ctx, s, img.TaskID); err != nil {
		return store.Image{}, err
	}

	kept, err := s.PutImage(ctx, img, r)
	if err != nil {
		return store.Image{}, fmt.Errorf("task %d: %w", img.TaskID, err)
	}

	if _, err := editable(ctx, s, img.TaskID); err != nil {
		return store.Image{}, errors.Join(err, s.RemoveImages(context.WithoutCancel(ctx), img.TaskID, []int64{kept.ID}))
	}

	return kept, nil
}

// RemoveImages removes the task's images ids, refused as EditTask is.
func RemoveImages(ctx context.Context, s Store, taskID int64, ids []int64) error {
	if _, err := editable(ctx, s, taskID); err != nil {
		return err
	}

	if err := s.RemoveImages(ctx, taskID, ids); err != nil {
		return fmt.Errorf("task %d: %w", taskID, err)
	}

	return nil
}
