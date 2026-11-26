package api

import (
	"bufio"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// sniffLen is how much of a file http.DetectContentType reads.
const sniffLen = 512

// uploadImage keeps the body as an image of the task, of the type its first bytes show.
func (s *server) uploadImage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	name, err := imageName(r.URL.Query().Get("name"))
	if err != nil {
		return err
	}

	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, engine.ImageMaxBytes))
	head, _ := body.Peek(sniffLen) // a read that failed fails again in AddImage

	typ := http.DetectContentType(head)
	if !slices.Contains(engine.ImageTypes, typ) {
		return &apiError{http.StatusUnsupportedMediaType, "the file is not a PNG, JPEG, GIF or WebP image"}
	}

	img, err := engine.AddImage(r.Context(), s.Store, store.Image{TaskID: id, Name: name, Type: typ}, body)

	switch _, tooLarge := errors.AsType[*http.MaxBytesError](err); {
	case tooLarge:
		return &apiError{http.StatusRequestEntityTooLarge, fmt.Sprintf("an image is at most %d MiB", engine.ImageMaxBytes>>20)}
	case errors.Is(err, engine.ErrRefused):
		return &apiError{http.StatusConflict, err.Error()}
	case err != nil && r.Context().Err() != nil:
		// The connection ended mid-body, as when a page is closed: the client's doing, so not logged.
		return &apiError{http.StatusBadRequest, "the upload ended before the image did"}
	case err != nil:
		return err
	}

	reply(w, http.StatusCreated, engine.Image{ID: img.ID, Name: img.Name, Type: img.Type, Size: img.Size})

	return nil
}

// imageName gives the base name of given with no control character, and
// refuses one with nothing left, one that names a directory, and one over 255 bytes.
func imageName(given string) (string, error) {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, given[strings.LastIndexAny(given, `/\`)+1:])

	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return "", &apiError{http.StatusBadRequest, "name is the image's file name, 1 to 255 bytes and not . or .."}
	}

	return name, nil
}

// image serves an image of the task as its stored type, never sniffed: shown
// in the page, or with ?download=1 saved under its name.
func (s *server) image(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}

	imageID, err := strconv.ParseInt(r.PathValue("image"), 10, 64)
	if err != nil {
		return &apiError{http.StatusNotFound, "the image's id in the path is not a number"}
	}

	img, file, err := s.Store.OpenImage(r.Context(), id, imageID)
	if err != nil {
		return err
	}

	defer file.Close()

	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}

	h := w.Header()
	// A shared cache could hand a response the session authorised to a request CAVU never checked.
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", img.Type)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": img.Name}))
	http.ServeContent(w, r, "", img.CreatedAt, file)

	return nil
}
