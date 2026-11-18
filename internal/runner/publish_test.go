package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/sandbox"
	"github.com/mhersson/cavu/internal/sidecar"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

const prevCommit = "fedcba9876543210fedcba9876543210fedcba98"

// The import's lists of a change that modifies a workflow and of one that deletes it.
const (
	workflowListed = "6 :100644 100644 1111111111111111111111111111111111111111 2222222222222222222222222222222222222222 M\t.github/workflows/ci.yml\n"
	workflowGone   = "missing :100644 000000 1111111111111111111111111111111111111111 0000000000000000000000000000000000000000 D\t.github/workflows/ci.yml\n"
)

// publishImportName is how the fake names the import of a bundle of the task
// branch at head, which must descend from prev.
func publishImportName(c engine.RunConfig, head, prev string) string {
	return stepName(scriptArgv(importScript, tmpfsBundle, c.Repo.Branch, head, prev, "refs/cavu/tasks/5"))
}

// toPublish is a fake whose import lists listed as the change's paths.
func toPublish(listed string) *fakeSandbox {
	f := newFake()
	f.out = map[string][]byte{changeFiles: []byte(listed)}

	return f
}

func firstPublish() engine.PublishRequest {
	return engine.PublishRequest{
		Bundle: []byte(exportedBundle), Head: headCommit, Branch: "cavu/fix-login", Title: "Fix login", Body: "The body.",
	}
}

func laterPublish() engine.PublishRequest {
	return engine.PublishRequest{Bundle: []byte(exportedBundle), Head: headCommit, Prev: prevCommit, Branch: "cavu/fix-login", Number: 3}
}

// appPublishConfig is appConfig for a run with a base commit.
func appPublishConfig(url string) engine.RunConfig {
	c := appConfig(url)
	c.Repo.BaseCommit = baseCommit

	return c
}

// pullForge returns a client of a forge on which no pull request is open and
// that opens each as number 7, a repository's address on it, and what each
// request to open one asked for.
func pullForge(t *testing.T) (*forge.Client, string, func() []map[string]any) {
	t.Helper()

	var opened struct {
		sync.Mutex
		asks []map[string]any
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/o/n/pulls", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })
	mux.HandleFunc("POST /api/v3/repos/o/n/pulls", func(w http.ResponseWriter, r *http.Request) {
		var ask map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&ask))

		opened.Lock()
		opened.asks = append(opened.asks, ask)
		opened.Unlock()

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://forge.example/o/n/pull/7","node_id":"PR_7"}`))
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	client, err := forge.New(forge.Options{Transport: srv.Client().Transport, Version: "test"})
	require.NoError(t, err)

	return client, srv.URL + "/o/n", func() []map[string]any {
		opened.Lock()
		defer opened.Unlock()

		return slices.Clone(opened.asks)
	}
}

// A first publish imports the bundle, pushes its head from the task's ref and
// opens a draft on the branch the push took. Only the change's list of paths
// leaves the import container, and no phase container comes up.
func TestPublishOpensOnTheBranchThePushTook(t *testing.T) {
	f := toPublish(twoFilesListed)
	f.onPush = func(r sidecar.PushRequest) (string, string, error) { return r.Branch + "-2", "", nil }

	client, url, opened := pullForge(t)
	r := f.runner()
	r.forge = client

	c := workConfig()
	c.Repo.URL = url

	res, err := r.Publish(t.Context(), c, firstPublish())
	require.NoError(t, err)
	assert.Equal(t, engine.PublishResult{Branch: "cavu/fix-login-2", Head: headCommit, Number: 7, URL: "https://forge.example/o/n/pull/7", NodeID: "PR_7"}, res)
	assert.Nil(t, r.state(c.RunID), "the sandbox opened for the publish is not kept")

	assert.Equal(t, []sidecar.PushRequest{{
		URL: url, Token: plantedForge, Ref: "refs/cavu/tasks/5", Commit: headCommit, Branch: "cavu/fix-login",
	}}, f.pushes)
	assert.Equal(t, []map[string]any{{
		"title": "Fix login", "body": "The body.", "base": "main", "head": "cavu/fix-login-2", "draft": true,
	}}, opened())

	assert.Equal(t, []string{
		"volumes", "importer", "import/copy", "import/run/" + importName(c, headCommit), "import/run/" + changeName(c, headCommit),
		"import/copyout " + changeFiles, "importer removed", "push",
	}, f.calls())
	assert.Equal(t, map[string][]byte{tmpfsBundle: []byte(exportedBundle)}, f.imported)
	assertImportCommands(t, f)
}

// A later publish pushes on top of the head published before and opens no
// pull request: with a personal access token it asks the forge nothing.
func TestPublishLaterOnlyPushes(t *testing.T) {
	f := toPublish(twoFilesListed)
	r, fake, url := f.appRunner(t)
	c := workConfig()
	c.Repo.URL = url

	res, err := r.Publish(t.Context(), c, laterPublish())
	require.NoError(t, err)
	assert.Equal(t, engine.PublishResult{Branch: "cavu/fix-login", Head: headCommit, Number: 3}, res)

	require.Len(t, f.pushes, 1)
	assert.Equal(t, prevCommit, f.pushes[0].Expect)
	assert.Contains(t, f.calls(), "import/run/"+publishImportName(c, headCommit, prevCommit), "the head must descend from the one published before")
	assert.Empty(t, fake.Requests())
}

// A run the runner holds publishes through its sandbox: no other is opened,
// and the phase that is up stays as it is.
func TestPublishThroughTheRunsSandbox(t *testing.T) {
	f := toPublish(twoFilesListed)
	r := f.runner()
	c := workConfig()

	_, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)

	opened, before := len(f.opened), len(f.calls())

	_, err = r.Publish(t.Context(), c, laterPublish())
	require.NoError(t, err)
	assert.Len(t, f.opened, opened)
	assert.Equal(t, []string{
		"importer", "import/copy", "import/run/" + publishImportName(c, headCommit, prevCommit), "import/run/" + changeName(c, headCommit),
		"import/copyout " + changeFiles, "importer removed", "push",
	}, f.calls()[before:])
}

// A GitHub App's push token may write contents, and workflows too only for a
// change that touches one, a deleted one included.
func TestPublishPushTokenPermissions(t *testing.T) {
	for name, tc := range map[string]struct {
		listed string
		perms  map[string]string
	}{
		"a change to a workflow":    {workflowListed, map[string]string{"contents": "write", "workflows": "write"}},
		"a change that deletes one": {workflowGone, map[string]string{"contents": "write", "workflows": "write"}},
		"any other change":          {twoFilesListed, map[string]string{"contents": "write"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := toPublish(tc.listed)
			r, fake, url := f.appRunner(t)

			_, err := r.Publish(t.Context(), appPublishConfig(url), laterPublish())
			require.NoError(t, err)
			require.Len(t, f.pushes, 1)
			assert.Equal(t, []forgefake.Token{{Value: f.pushes[0].Token, Permissions: tc.perms}}, fake.Tokens())
		})
	}
}

// A bundle the import refuses, a push the forge refuses and a push token the
// forge will not mint are each a rejection, and what would follow does not run.
func TestPublishRejections(t *testing.T) {
	t.Run("a bundle the import refuses", func(t *testing.T) {
		f := toPublish(twoFilesListed)
		c := workConfig()
		f.results[importName(c, headCommit)] = sandbox.Result{ExitCode: 1, Output: "refused " + plantedSetup + "\n"}

		res, err := f.runner().Publish(t.Context(), c, firstPublish())
		require.NoError(t, err)
		assert.Equal(t, engine.PublishResult{Rejected: masker(c)("refused " + plantedSetup)}, res)
		assert.Empty(t, f.pushes)
	})

	t.Run("a push the forge refuses", func(t *testing.T) {
		const moved = "the branch moved since it was pushed"

		f := toPublish(twoFilesListed)
		f.onPush = func(r sidecar.PushRequest) (string, string, error) { return r.Branch, moved, nil }

		client, url, opened := pullForge(t)
		r := f.runner()
		r.forge = client

		c := workConfig()
		c.Repo.URL = url

		res, err := r.Publish(t.Context(), c, firstPublish())
		require.NoError(t, err)
		assert.Equal(t, engine.PublishResult{Rejected: moved}, res)
		assert.Empty(t, opened())
	})

	t.Run("a push token the forge will not mint", func(t *testing.T) {
		f := toPublish(workflowListed)
		r, fake, url := f.appRunner(t)
		fake.Lack("workflows")

		res, err := r.Publish(t.Context(), appPublishConfig(url), firstPublish())
		require.NoError(t, err)
		assert.Contains(t, res.Rejected, "Workflows")
		assert.Empty(t, f.pushes)
	})

	t.Run("a mint that fails otherwise is an error", func(t *testing.T) {
		f := toPublish(workflowListed)
		r, fake, url := f.appRunner(t)
		fake.Fail(1)

		res, err := r.Publish(t.Context(), appPublishConfig(url), firstPublish())
		require.Error(t, err)
		assert.Empty(t, res.Rejected)
		assert.Empty(t, f.pushes)
	})
}

// A token the App minted is in no config: an error or a rejection that quotes
// it is masked.
func TestPublishMasksAMintedToken(t *testing.T) {
	tests := []struct {
		name    string
		push    func(sidecar.PushRequest) (string, string, error)
		wantErr bool
	}{
		{"an error", func(r sidecar.PushRequest) (string, string, error) {
			return "", "", fmt.Errorf("fatal: could not read from https://x-access-token:%s@forge.example/o/n", r.Token)
		}, true},
		{"a rejection", func(r sidecar.PushRequest) (string, string, error) {
			return r.Branch, "refused " + r.Token, nil
		}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := toPublish(twoFilesListed)
			f.onPush = tc.push
			r, fake, url := f.appRunner(t)

			res, err := r.Publish(t.Context(), appPublishConfig(url), laterPublish())
			require.Len(t, f.pushes, 1)

			minted := fake.Tokens()
			require.Len(t, minted, 1)
			assert.Equal(t, minted[0].Value, f.pushes[0].Token)

			text := res.Rejected

			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, res.Rejected)

				text = err.Error()
			} else {
				require.NoError(t, err)
			}

			assert.NotContains(t, text, minted[0].Value)
			assert.Contains(t, text, "[redacted]")
		})
	}
}
