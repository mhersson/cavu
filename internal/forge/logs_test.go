package forge

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

const (
	logPath    = "/api/v3/repos/o/n/actions/jobs/7/logs"
	logStorage = "logs.ghe.example"
)

func failedJob(id int64, name string) forgefake.CheckRun {
	return forgefake.CheckRun{ID: id, App: "github-actions", Name: name, Status: "completed", Conclusion: "failure"}
}

// numbered is a log of n bytes whose lines are numbered, so any tail of it
// appears nowhere else in it.
func numbered(n int) string {
	var b strings.Builder

	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "line %07d\n", i)
	}

	return b.String()[:n]
}

func TestFailedLogsAreBounded(t *testing.T) {
	bare, head := newBareRepo(t)
	big := numbered(1 << 20)
	tail := big[len(big)-32<<10:]

	t.Run("a 1 MiB log is cut to its last 32 KiB", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetChecks(head, failedJob(7, "unit-tests"))
		f.SetJobLog(7, big)

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.NoError(t, err)
		assert.Contains(t, out, "unit-tests")
		assert.Contains(t, out, tail)
		assert.NotContains(t, out, big[len(big)-32<<10-1:])

		for _, r := range f.Requests() {
			if r.Path == logPath {
				assert.Equal(t, "bytes=-32768", r.Header.Get("Range"))
			}
		}
	})

	t.Run("a log from storage that ignores the range", func(t *testing.T) {
		var (
			mu      sync.Mutex
			headers []http.Header
		)

		storage := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			headers = append(headers, r.Header.Clone())
			_, _ = w.Write([]byte(big))
		})

		f := forgefake.New(t, bare)
		f.SetChecks(head, failedJob(7, "unit-tests"))

		mux := http.NewServeMux()
		mux.Handle("/", f)
		mux.HandleFunc("GET "+logPath, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+logStorage+"/log?sig=signed", http.StatusFound)
		})

		c := newTestClient(t, nil, fakeRepo.Host, mux, map[string]http.Handler{logStorage: storage})

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.NoError(t, err)
		assert.Contains(t, out, tail)
		assert.NotContains(t, out, big[len(big)-32<<10-1:])

		mu.Lock()
		defer mu.Unlock()

		require.Len(t, headers, 1)
		assert.Equal(t, "bytes=-32768", headers[0].Get("Range"))
		assert.Empty(t, headers[0].Get("Authorization"))
	})

	t.Run("an error keeps the storage address out", func(t *testing.T) {
		f := forgefake.New(t, bare)
		f.SetChecks(head, failedJob(7, "unit-tests"))

		mux := http.NewServeMux()
		mux.Handle("/", f)
		mux.HandleFunc("GET "+logPath, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+logStorage+"/log?sig=signed", http.StatusFound)
		})

		gone := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusNotFound) })
		c := newTestClient(t, nil, fakeRepo.Host, mux, map[string]http.Handler{logStorage: gone})

		_, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "sig=signed")
		assert.NotContains(t, err.Error(), logStorage)
	})

	t.Run("an empty log", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetChecks(head, failedJob(7, "unit-tests"))
		f.SetJobLog(7, "")

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.NoError(t, err)
		assert.Contains(t, out, "unit-tests")
	})

	t.Run("five failed jobs stop at 64 KiB and the last line names those left out", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetChecks(head, failedJob(3, "job-3"), failedJob(1, "job-1"), failedJob(5, "job-5"),
			failedJob(2, "job-2"), failedJob(4, "job-4"), run("passing-check", "completed", "success"))

		// Logs of about a third of the cap, where the last line decides whether
		// a third entry still fits.
		for size := 21_600; size < 22_000; size += 3 {
			logs := make([]string, 5)
			for i := range logs {
				logs[i] = strings.Repeat(string(rune('a'+i)), size)
				f.SetJobLog(int64(i+1), logs[i])
			}

			out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
			require.NoError(t, err)
			require.LessOrEqual(t, len(out), logsCap, "logs of %d bytes", size)
			assert.NotContains(t, out, "passing-check")

			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			last := lines[len(lines)-1]
			whole, at := 0, -1

			for i, l := range logs {
				name := fmt.Sprintf("job-%d", i+1)

				if j := strings.Index(out, l); j >= 0 {
					require.Equal(t, whole, i, "logs of %d bytes: whole entries are the first in name order", size)
					require.Greater(t, j, at)
					require.NotContains(t, last, name)

					whole, at = whole+1, j

					continue
				}

				require.NotContains(t, out, l[:64], "logs of %d bytes: %s is whole or left out", size, name)
				require.Contains(t, last, name)
			}

			if size == 21_600 {
				assert.Equal(t, 3, whole)
			}
		}
	})

	t.Run("a failed status with no log gives its name and address", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetStatus(head, forgefake.Status{Context: "ci/jenkins", State: "failure", URL: "https://ci.example/build/9"})

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.NoError(t, err)
		assert.Contains(t, out, "ci/jenkins")
		assert.Contains(t, out, "https://ci.example/build/9")
	})

	t.Run("a failed check of another app gives its title, summary and address", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetChecks(head, forgefake.CheckRun{
			ID: 8, App: "codecov", Name: "coverage", Status: "completed", Conclusion: "failure",
			Title: "Coverage fell", Summary: "81% of lines", URL: "https://codecov.example/9",
		})

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, nil)
		require.NoError(t, err)

		for _, s := range []string{"coverage", "Coverage fell", "81% of lines", "https://codecov.example/9"} {
			assert.Contains(t, out, s)
		}

		assert.Zero(t, sent(f, http.MethodGet, "/api/v3/repos/o/n/actions/jobs/8/logs"))
	})

	t.Run("an ignored check is left out", func(t *testing.T) {
		f, c := newFake(t, bare, nil)
		f.SetChecks(head, failedJob(7, "flaky"))
		f.SetJobLog(7, "boom")

		out, err := c.FailedLogs(t.Context(), fakeRepo, fakeAccess, head, []string{"flaky"})
		require.NoError(t, err)
		assert.Empty(t, out)
		assert.Zero(t, sent(f, http.MethodGet, logPath))
	})
}
