package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/store"
)

// shot is n bytes that begin as a PNG does, so the server types them image/png.
func shot(n int) string {
	const signature = "\x89PNG\r\n\x1a\n"

	return signature + strings.Repeat("x", n-len(signature))
}

// upload sends contents as an image of the task under name and returns the status and the body.
func (f *fixture) upload(t *testing.T, taskID int64, name, contents string) (int, string) {
	t.Helper()

	return f.call(t, fmt.Sprintf("POST /api/tasks/%d/images?name=%s", taskID, url.QueryEscape(name)), contents)
}

// images lists the task's stored images.
func (f *fixture) images(t *testing.T, taskID int64) []store.Image {
	t.Helper()

	imgs, err := f.store.TaskImages(t.Context(), taskID)
	require.NoError(t, err)

	return imgs
}

// imageFiles lists the files in the images directory beside the store's file.
func (f *fixture) imageFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(filepath.Dir(f.db), "images"))
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

// viewImages lists the images the task's view shows, each as its JSON object.
func (f *fixture) viewImages(t *testing.T, taskID int64) []any {
	t.Helper()

	imgs, ok := f.view(t, taskID)["images"].([]any)
	require.True(t, ok)

	return imgs
}

func TestUploadImage(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	status, body := f.upload(t, id, "save-button.png", shot(300))
	require.Equal(t, http.StatusCreated, status, body)

	first := object(t, body)
	assert.Equal(t, map[string]any{"id": first["id"], "name": "save-button.png", "type": "image/png", "size": float64(300)}, first)
	assert.Equal(t, []any{first}, f.viewImages(t, id))

	status, body = f.upload(t, id, "again.png", shot(300))
	require.Equal(t, http.StatusCreated, status, body)
	assert.NotEqual(t, first["id"], object(t, body)["id"])
	assert.Len(t, f.viewImages(t, id), 2, "the same contents twice are two images")
	assert.Len(t, f.imageFiles(t), 1, "and one file")

	t.Run("typed by its contents, never by its name", func(t *testing.T) {
		for want, contents := range map[string]string{
			"image/jpeg": "\xff\xd8\xff\xe0" + strings.Repeat("j", 100),
			"image/gif":  "GIF89a" + strings.Repeat("g", 100),
			"image/webp": "RIFF\x64\x00\x00\x00WEBPVP8 " + strings.Repeat("w", 100),
		} {
			status, body := f.upload(t, id, "shot.png", contents)
			require.Equal(t, http.StatusCreated, status, body)
			assert.Equal(t, want, object(t, body)["type"])
		}
	})

	t.Run("the name is its base name without control characters", func(t *testing.T) {
		for given, want := range map[string]string{
			"screens/today/menu.png": "menu.png",
			`C:\Users\ada\menu.png`:  "menu.png",
			"me\x00n\x1bu.png":       "menu.png",
			"skjermbilde ø.png":      "skjermbilde ø.png",
		} {
			status, body := f.upload(t, id, given, shot(50))
			require.Equal(t, http.StatusCreated, status, body)
			assert.Equal(t, want, object(t, body)["name"], given)
		}
	})

	t.Run("a name with nothing left is refused", func(t *testing.T) {
		before := f.images(t, id)

		for _, given := range []string{"", "shots/", "\x01\x02", ".", "..", strings.Repeat("n", 252) + ".png"} {
			status, body := f.upload(t, id, given, shot(50))
			require.Equal(t, http.StatusBadRequest, status, "%q: %s", given, body)
			assertError(t, body)
		}

		assert.Equal(t, before, f.images(t, id))
	})
}

func TestUploadRefusesOverTheCap(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	status, body := f.upload(t, id, "over.png", shot(int(engine.ImageMaxBytes)+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, status, body)
	assertError(t, body)
	assert.Empty(t, f.images(t, id))
	assert.Empty(t, f.imageFiles(t), "nothing is left behind")

	status, body = f.upload(t, id, "at.png", shot(int(engine.ImageMaxBytes)))
	require.Equal(t, http.StatusCreated, status, body)
	assert.EqualValues(t, engine.ImageMaxBytes, object(t, body)["size"], "the cap itself is taken")
}

func TestUploadRefusesPastTheTaskCap(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)
	full := int(engine.ImageMaxBytes)

	for left := int(engine.TaskImagesMaxBytes); left > 0; left -= min(left, full) {
		status, body := f.upload(t, id, "part.png", shot(min(left, full)))
		require.Equal(t, http.StatusCreated, status, body)
	}

	files := f.imageFiles(t)

	r := f.request(t, fmt.Sprintf("POST /api/tasks/%d/images?name=one-more.png", id), shot(1<<20))
	r.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.srv.Client().Do(r)
	require.NoError(t, err)

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assertError(t, string(body))
	assert.False(t, resp.Close, "closing mid-upload can hide the response from the browser")
	assert.Equal(t, engine.TaskImagesMaxBytes, held(f.images(t, id)), "the cap itself is taken, and not a byte more")
	assert.Equal(t, files, f.imageFiles(t), "nothing is left behind")

	other := f.task(t, `{"project": "site", "title": "Fix the cancel button"}`)
	status, got := f.upload(t, other, "shot.png", shot(8))
	require.Equal(t, http.StatusCreated, status, got)
}

// Uploads sent at once keep the task within its cap, however they interleave.
func TestConcurrentUploadsKeepTheTaskCap(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	const (
		uploads = 5
		size    = 30 << 10
		fit     = 3
	)

	// Room for three of the uploads and not for a fourth.
	room := int64(fit*size + size/2)
	_, err := engine.AddImage(t.Context(), f.store, store.Image{TaskID: id, Name: "big.png", Type: "image/png"},
		bytes.NewReader(make([]byte, engine.TaskImagesMaxBytes-room)))
	require.NoError(t, err)

	requests := make([]*http.Request, uploads)
	for i := range requests {
		requests[i] = f.request(t, fmt.Sprintf("POST /api/tasks/%d/images?name=shot.png", id), shot(size-i))
		requests[i].Header.Set("Authorization", "Bearer "+f.token)
	}

	statuses := make([]int, uploads)
	errs := make([]error, uploads)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	var wg sync.WaitGroup

	for i, r := range requests {
		wg.Go(func() {
			resp, err := client.Do(r)
			if err != nil {
				errs[i] = err

				return
			}

			_, errs[i] = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			statuses[i] = resp.StatusCode
		})
	}

	wg.Wait()
	require.NoError(t, errors.Join(errs...))

	counts := map[int]int{}
	for _, s := range statuses {
		counts[s]++
	}

	assert.Equal(t, map[int]int{http.StatusCreated: fit, http.StatusRequestEntityTooLarge: uploads - fit}, counts)
	assert.Len(t, f.images(t, id), 1+fit)
	assert.LessOrEqual(t, held(f.images(t, id)), engine.TaskImagesMaxBytes)
	assert.Len(t, f.imageFiles(t), 1+fit, "a refused upload's contents are not kept")
}

// held is what imgs hold together.
func held(imgs []store.Image) int64 {
	var n int64
	for _, img := range imgs {
		n += img.Size
	}

	return n
}

func TestUploadRefusesAnotherType(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	for name, contents := range map[string]string{
		"shot.png":  "The save button does nothing.\n",
		"IMG_1.png": "\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic" + strings.Repeat("h", 100),
		"page.png":  "<svg xmlns=\"http://www.w3.org/2000/svg\"><script>alert(1)</script></svg>",
		"empty.png": "",
	} {
		status, body := f.upload(t, id, name, contents)
		require.Equal(t, http.StatusUnsupportedMediaType, status, "%s: %s", name, body)
		assertError(t, body)
	}

	assert.Empty(t, f.images(t, id))
	assert.Empty(t, f.imageFiles(t))
}

func TestUploadRefusedOnceARunStarted(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)

	running := f.task(t, `{"project": "site", "title": "Running"}`)
	f.add(t, running, engine.TaskStarted, struct{}{})
	f.add(t, running, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})

	cancelled := f.task(t, `{"project": "site", "title": "Cancelled"}`)
	status, body := f.call(t, fmt.Sprintf("POST /api/tasks/%d/actions", cancelled), `{"action": "cancel"}`)
	require.Equal(t, http.StatusOK, status, body)

	for _, id := range []int64{running, cancelled} {
		status, body := f.upload(t, id, "shot.png", shot(100))
		require.Equal(t, http.StatusConflict, status, body)
		assertError(t, body)
		assert.Empty(t, f.images(t, id))
	}

	status, body = f.upload(t, 99, "shot.png", shot(100))
	require.Equal(t, http.StatusNotFound, status, body)
	assertError(t, body)
	assert.Empty(t, f.imageFiles(t))
}

func TestUploadThatEndsEarly(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", f.srv.Listener.Addr().String())
	require.NoError(t, err)

	defer conn.Close()

	// The body stops at 600 of the 1000 bytes it declares, as when a page is closed mid-upload.
	_, err = fmt.Fprintf(conn, "POST /api/tasks/%d/images?name=shot.png HTTP/1.1\r\nHost: cavu\r\nAuthorization: Bearer %s\r\n"+
		"Content-Length: 1000\r\n\r\n%s", id, f.token, shot(600))
	require.NoError(t, err)

	tcp, ok := conn.(*net.TCPConn)
	require.True(t, ok)
	require.NoError(t, tcp.CloseWrite())

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "the client's doing, not a failure of the server")
	assert.Empty(t, f.images(t, id))
	assert.Empty(t, f.imageFiles(t))
}

func TestServeImage(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)
	other := f.task(t, `{"project": "site", "title": "Another"}`)

	contents := shot(400)
	status, body := f.upload(t, id, `save "button" ø.png`, contents)
	require.Equal(t, http.StatusCreated, status, body)

	image := int64(object(t, body)["id"].(float64))
	route := fmt.Sprintf("GET /api/tasks/%d/images/%d", id, image)

	get := func(route string, byteRange ...string) (*http.Response, string) {
		r := f.request(t, route, "")
		r.Header.Set("Authorization", "Bearer "+f.token)

		if len(byteRange) > 0 {
			r.Header.Set("Range", byteRange[0])
		}

		return send(t, r)
	}

	disposition := func(resp *http.Response) (string, string) {
		kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		require.NoError(t, err)

		return kind, params["filename"]
	}

	resp, got := get(route)
	require.Equal(t, http.StatusOK, resp.StatusCode, got)
	assert.Equal(t, contents, got)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))

	kind, name := disposition(resp)
	assert.Equal(t, "inline", kind)
	assert.Equal(t, `save "button" ø.png`, name)

	resp, got = get(route, "bytes=0-7")
	require.Equal(t, http.StatusPartialContent, resp.StatusCode, got)
	assert.Equal(t, contents[:8], got)

	resp, got = get("HEAD" + strings.TrimPrefix(route, "GET"))
	require.Equal(t, http.StatusOK, resp.StatusCode, got)
	assert.Empty(t, got)
	assert.Equal(t, "400", resp.Header.Get("Content-Length"))

	resp, got = get(route + "?download=1")
	require.Equal(t, http.StatusOK, resp.StatusCode, got)
	assert.Equal(t, contents, got)

	kind, name = disposition(resp)
	assert.Equal(t, "attachment", kind)
	assert.Equal(t, `save "button" ø.png`, name, "the name it was given")

	for _, absent := range []string{
		fmt.Sprintf("GET /api/tasks/%d/images/%d", other, image),
		fmt.Sprintf("GET /api/tasks/%d/images/%d", id, image+1),
		fmt.Sprintf("GET /api/tasks/%d/images/first", id),
		fmt.Sprintf("GET /api/tasks/99/images/%d", image),
	} {
		resp, got := get(absent)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, absent)
		assertError(t, got)
	}

	resp, _ = send(t, f.request(t, route, ""))
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestServedImageIsNeverStored(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)

	contents := shot(400)
	status, body := f.upload(t, id, "save-button.png", contents)
	require.Equal(t, http.StatusCreated, status, body)

	path := fmt.Sprintf("/api/tasks/%d/images/%d", id, int64(object(t, body)["id"].(float64)))
	session := f.login(t).Value

	for _, c := range []struct {
		name, route, byteRange string
		status                 int
		body, disposition      string
		contentRange           string
	}{
		{name: "shown", route: "GET " + path, status: http.StatusOK, body: contents, disposition: "inline"},
		{name: "downloaded", route: "GET " + path + "?download=1", status: http.StatusOK, body: contents, disposition: "attachment"},
		{name: "HEAD", route: "HEAD " + path, status: http.StatusOK, disposition: "inline"},
		{name: "HEAD of a download", route: "HEAD " + path + "?download=1", status: http.StatusOK, disposition: "attachment"},
		{
			name: "part", route: "GET " + path, byteRange: "bytes=8-15", status: http.StatusPartialContent,
			body: contents[8:16], disposition: "inline", contentRange: "bytes 8-15/400",
		},
		{
			name: "part of a download", route: "GET " + path + "?download=1", byteRange: "bytes=0-7", status: http.StatusPartialContent,
			body: contents[:8], disposition: "attachment", contentRange: "bytes 0-7/400",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := withCookie(f.request(t, c.route, ""), session, false)
			if c.byteRange != "" {
				r.Header.Set("Range", c.byteRange)
			}

			resp, got := send(t, r)
			require.Equal(t, c.status, resp.StatusCode, got)
			assert.Equal(t, c.body, got)
			assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
			assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
			assert.Equal(t, c.contentRange, resp.Header.Get("Content-Range"))

			kind, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
			require.NoError(t, err)
			assert.Equal(t, c.disposition, kind)
			assert.Equal(t, "save-button.png", params["filename"])
		})
	}
}

func TestEditRemovesImages(t *testing.T) {
	f := newServer(t)
	f.addProject(t, site)
	first := f.task(t, `{"project": "site", "title": "Write the copy"}`)
	second := f.task(t, `{"project": "site", "title": "Pick the colours"}`)
	id := f.task(t, `{"project": "site", "title": "Fix the save button"}`)
	patch := fmt.Sprintf("PATCH /api/tasks/%d", id)

	var ids []int64

	for _, n := range []int{100, 200, 300} {
		status, body := f.upload(t, id, "shot.png", shot(n))
		require.Equal(t, http.StatusCreated, status, body)

		ids = append(ids, int64(object(t, body)["id"].(float64)))
	}

	imageIDs := func(view map[string]any) []int64 {
		out := []int64{}
		for _, img := range view["images"].([]any) {
			out = append(out, int64(img.(map[string]any)["id"].(float64)))
		}

		return out
	}

	status, body := f.call(t, patch, fmt.Sprintf(`{"title": "Renamed", "body": "It does nothing.", "depends_on": [%d], "remove_images": [%d]}`, first, ids[0]))
	require.Equal(t, http.StatusOK, status, body)

	got := object(t, body)
	assert.Equal(t, "Renamed", got["title"])
	assert.Equal(t, "It does nothing.", got["body"])
	assert.Equal(t, []int64{first}, depIDs(got))
	assert.Equal(t, []int64{ids[1], ids[2]}, imageIDs(got))
	assert.Equal(t, f.view(t, id), got, "the task as it is stored")

	got = tool(t, f.client(t), "edit_task", map[string]any{
		"id": id, "title": "Renamed again", "body": "It saves nothing.", "depends_on": []int64{second}, "remove_images": []int64{ids[1], ids[2]},
	})
	assert.Equal(t, "Renamed again", got["title"])
	assert.Equal(t, "It saves nothing.", got["body"])
	assert.Equal(t, []int64{second}, depIDs(got))
	assert.Empty(t, imageIDs(got))
	assert.Equal(t, f.view(t, id), got, "the task as REST shows it")
	assert.Empty(t, f.imageFiles(t), "contents no image names are gone")

	t.Run("refused", func(t *testing.T) {
		status, body := f.upload(t, id, "kept.png", shot(400))
		require.Equal(t, http.StatusCreated, status, body)

		kept := int64(object(t, body)["id"].(float64))
		other := f.task(t, `{"project": "site", "title": "Another"}`)

		// unchanged wants every task as it was when unchanged was made, and
		// the image kept with its contents.
		unchanged := func() func(msg string) {
			record, view, files := f.record(t), f.view(t, id), f.imageFiles(t)

			return func(msg string) {
				t.Helper()

				assert.Equal(t, record, f.record(t), msg)
				assert.Equal(t, view, f.view(t, id), msg)
				assert.Equal(t, files, f.imageFiles(t), msg)

				status, contents := f.call(t, fmt.Sprintf("GET /api/tasks/%d/images/%d", id, kept), "")
				require.Equal(t, http.StatusOK, status, msg)
				assert.Equal(t, shot(400), contents, msg)
			}
		}()

		status, body = f.call(t, fmt.Sprintf("PATCH /api/tasks/%d", other), fmt.Sprintf(`{"remove_images": [%d]}`, kept))
		require.Equal(t, http.StatusNotFound, status, "another task's image: %s", body)
		unchanged("another task's image")

		status, body = f.call(t, patch, fmt.Sprintf(`{"remove_images": [%d, %d]}`, kept, kept+1))
		require.Equal(t, http.StatusNotFound, status, "one the task lacks: %s", body)
		unchanged("removes none")

		mixed := map[string]any{"title": "Changed", "body": "Changed too.", "depends_on": []int64{other}, "remove_images": []int64{kept, kept + 1}}
		raw, err := json.Marshal(mixed)
		require.NoError(t, err)

		status, body = f.call(t, patch, string(raw))
		require.Equal(t, http.StatusNotFound, status, "a mixed edit naming an image the task lacks: %s", body)
		assertError(t, body)
		unchanged("a mixed edit naming an image the task lacks writes none of it")

		mixed["id"] = id
		res, err := f.client(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "edit_task", Arguments: mixed})
		require.NoError(t, err)
		refused(t, res)
		unchanged("the same edit over MCP writes none of it")

		status, body = f.call(t, patch, fmt.Sprintf(`{"title": "", "remove_images": [%d]}`, kept))
		require.Equal(t, http.StatusBadRequest, status, "a title the edit refuses: %s", body)
		unchanged("a refused title removes no image")

		f.add(t, id, engine.TaskStarted, struct{}{})
		f.add(t, id, engine.RunStarted, engine.RunStartedPayload{Reason: "initial"})

		status, body = f.call(t, patch, fmt.Sprintf(`{"remove_images": [%d]}`, kept))
		require.Equal(t, http.StatusBadRequest, status, "once a run started, as every edit: %s", body)
		assertError(t, body)
		assert.Equal(t, []int64{kept}, imageIDs(f.view(t, id)))
	})
}
