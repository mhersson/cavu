//go:build integration

package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/engine"
	"github.com/mhersson/cavu/internal/forge"
	"github.com/mhersson/cavu/internal/testkit/forgefake"
	"github.com/mhersson/cavu/internal/testkit/gitrepo"
	"github.com/mhersson/cavu/internal/testkit/sandboxtest"
)

const publishToken = "ghp_publishtoken0123456789abcdef" //nolint:gosec // a test token

const workflowChange = "mkdir -p .github/workflows && echo 'on: push' > .github/workflows/ci.yml"

var appAccess = engine.ForgeAccess{AppID: 42, AppKey: forgefake.AppKey()}

// publishFixture is a run prepared from a repository holding notes.txt, whose
// one TLS server serves the repository's git under /o/ and a fake forge under
// /api/. The runner's forge client reaches that server from the host.
type publishFixture struct {
	r    *Runner
	c    engine.RunConfig
	fake *forgefake.Fake
	bare string

	mu        sync.Mutex
	passwords []string // of each git request's basic auth, in order
}

func newPublishFixture(t *testing.T, access engine.ForgeAccess) *publishFixture {
	t.Helper()

	px := &publishFixture{}

	r, c, _ := fixtureRunOn(t, map[string]string{"notes.txt": "first line\n"}, func(bare string) (http.Handler, string) {
		gitrepo.AllowPush(t, bare)
		px.bare, px.fake = bare, forgefake.New(t, bare)

		mux := http.NewServeMux()
		mux.Handle("/api/", px.fake)
		mux.Handle("/o/", http.StripPrefix("/o", px.authed(access.Token, gitrepo.Handler(t, bare))))

		return mux, "/o/" + filepath.Base(bare)
	})

	hostAddr := sandboxtest.HostAddr(t, r.rt)
	caPEM, _, _ := sandboxtest.CA(t)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	// Every dial goes to the fixture's server, so no request leaves the host.
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}

			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(hostAddr, port))
		},
	}
	t.Cleanup(transport.CloseIdleConnections)

	client, err := forge.New(forge.Options{Transport: transport, Version: "test"})
	require.NoError(t, err)

	r.forge = client
	c.Repo.Access = access
	c.Verify.Waived = true

	res, err := r.Prepare(t.Context(), c)
	require.NoError(t, err)
	require.Empty(t, res.FailedStep, res.Output)

	c.Repo.BaseCommit, c.Ecosystems = res.BaseCommit, res.Ecosystems
	px.r, px.c = r, c

	return px
}

// authed records the password of each request's basic auth and serves h to
// one whose password is pat or a token the fake minted.
func (px *publishFixture) authed(pat string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		user, pass, ok := req.BasicAuth()
		if ok {
			px.mu.Lock()
			px.passwords = append(px.passwords, pass)
			px.mu.Unlock()
		}

		minted := slices.ContainsFunc(px.fake.Tokens(), func(tok forgefake.Token) bool { return tok.Value == pass })
		if !ok || user != "x-access-token" || pass != pat && !minted {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		h.ServeHTTP(w, req)
	})
}

// exported commits what script changes in the agent phase and exports it.
func (px *publishFixture) exported(t *testing.T, script string) engine.ExportResult {
	t.Helper()

	head := commit(t, px.r.state(px.c.RunID), px.c, script)

	res, err := px.r.Export(t.Context(), px.c, head)
	require.NoError(t, err)
	require.Empty(t, res.Problem)

	return res
}

// first is the request of the task's first publish of exp.
func (px *publishFixture) first(exp engine.ExportResult) engine.PublishRequest {
	return engine.PublishRequest{Bundle: exp.Bundle, Head: exp.HeadCommit, Branch: px.c.Repo.Branch, Title: "Fix the notes", Body: "The notes are fixed."}
}

// publish publishes p and returns the result and the passwords the git server
// saw meanwhile.
func (px *publishFixture) publish(t *testing.T, p engine.PublishRequest) (engine.PublishResult, []string) {
	t.Helper()

	before := len(px.seen())

	res, err := px.r.Publish(t.Context(), px.c, p)
	require.NoError(t, err)

	return res, px.seen()[before:]
}

func (px *publishFixture) seen() []string {
	px.mu.Lock()
	defer px.mu.Unlock()

	return slices.Clone(px.passwords)
}

// branchAt is the commit of the task's branch on the forge, "" for none.
func (px *publishFixture) branchAt(t *testing.T) string {
	t.Helper()

	return gitrepo.Git(t, px.bare, "for-each-ref", "--format=%(objectname)", "refs/heads/"+px.c.Repo.Branch)
}

func TestPublishPushesAndOpensADraft(t *testing.T) {
	px := newPublishFixture(t, engine.ForgeAccess{Token: publishToken})
	exp := px.exported(t, "echo CAVU-PUBLISHED >> notes.txt")

	res, seen := px.publish(t, px.first(exp))
	require.Empty(t, res.Rejected)
	assert.Equal(t, exp.HeadCommit, px.branchAt(t))

	pulls := px.fake.Pulls()
	require.Len(t, pulls, 1)
	assert.Equal(t, forgefake.Pull{
		Number: pulls[0].Number, NodeID: pulls[0].NodeID, Title: "Fix the notes", Body: "The notes are fixed.",
		Base: "main", Head: px.c.Repo.Branch, Draft: true, State: "open",
	}, pulls[0])
	assert.Equal(t, engine.PublishResult{
		Branch: px.c.Repo.Branch, Head: exp.HeadCommit, Number: pulls[0].Number, NodeID: pulls[0].NodeID,
		URL: strings.TrimSuffix(px.c.Repo.URL, ".git") + "/pull/" + strconv.Itoa(pulls[0].Number),
	}, res)

	require.NotEmpty(t, seen)

	for _, p := range seen {
		assert.Equal(t, publishToken, p, "a personal access token goes to the push as it is")
	}
}

func TestPublishIsRepeatable(t *testing.T) {
	px := newPublishFixture(t, engine.ForgeAccess{Token: publishToken})
	exp := px.exported(t, "echo CAVU-PUBLISHED >> notes.txt")

	first, _ := px.publish(t, px.first(exp))
	require.Empty(t, first.Rejected)

	again, _ := px.publish(t, px.first(exp))
	assert.Equal(t, first, again)
	assert.Len(t, px.fake.Pulls(), 1)
	assert.Equal(t, exp.HeadCommit, px.branchAt(t))
}

func TestPublishWithAPullRequestOnlyPushes(t *testing.T) {
	px := newPublishFixture(t, engine.ForgeAccess{Token: publishToken})

	first, _ := px.publish(t, px.first(px.exported(t, "echo CAVU-FIRST >> notes.txt")))
	require.Empty(t, first.Rejected)

	sent := len(px.fake.Requests())
	next := px.exported(t, "echo CAVU-SECOND >> notes.txt")

	res, _ := px.publish(t, engine.PublishRequest{
		Bundle: next.Bundle, Head: next.HeadCommit, Prev: first.Head, Branch: first.Branch, Number: first.Number,
	})
	assert.Equal(t, engine.PublishResult{Branch: first.Branch, Head: next.HeadCommit, Number: first.Number}, res)
	assert.Equal(t, next.HeadCommit, px.branchAt(t))
	assert.Len(t, px.fake.Requests(), sent, "a personal access token's later publish asks the forge nothing")
	assert.Len(t, px.fake.Pulls(), 1)
}

// The gate tested one commit and the bundle holds a later one: nothing is pushed.
func TestPublishRejectsABundleTheGateDidNotTest(t *testing.T) {
	px := newPublishFixture(t, engine.ForgeAccess{Token: publishToken})

	tested := commit(t, px.r.state(px.c.RunID), px.c, "echo CAVU-TESTED >> notes.txt")
	later := px.exported(t, "echo CAVU-LATER >> notes.txt")

	req := px.first(later)
	req.Head = tested

	res, seen := px.publish(t, req)
	assert.Contains(t, res.Rejected, tested)
	assert.Contains(t, res.Rejected, later.HeadCommit)
	assert.Empty(t, seen, "the forge was not asked")
	assert.Empty(t, px.branchAt(t))
	assert.Empty(t, px.fake.Pulls())
}

func TestPublishMintsAPushTokenForAnApp(t *testing.T) {
	px := newPublishFixture(t, appAccess)
	exp := px.exported(t, "echo CAVU-PUBLISHED >> notes.txt")

	res, seen := px.publish(t, px.first(exp))
	require.Empty(t, res.Rejected)
	assert.Equal(t, exp.HeadCommit, px.branchAt(t))

	// The clone's token, which the API calls reuse, then the push's.
	minted := px.fake.Tokens()
	require.Len(t, minted, 2)

	api, push := minted[0], minted[1]
	assert.Equal(t, map[string]string{"contents": "write"}, push.Permissions)
	assert.NotEqual(t, api.Value, push.Value)

	require.NotEmpty(t, seen)

	for _, p := range seen {
		assert.Equal(t, push.Value, p)
	}

	var calls int

	for _, req := range px.fake.Requests() {
		if strings.Contains(req.Path, "/pulls") {
			calls++

			assert.Equal(t, "Bearer "+api.Value, req.Header.Get("Authorization"), req.Path)
		}
	}

	assert.Positive(t, calls)
}

func TestPublishAsksForWorkflowsOnlyForAWorkflowChange(t *testing.T) {
	for name, tc := range map[string]struct {
		script string
		perms  map[string]string
	}{
		"a workflow change": {workflowChange, map[string]string{"contents": "write", "workflows": "write"}},
		"another change":    {"echo CAVU-OTHER >> notes.txt", map[string]string{"contents": "write"}},
	} {
		t.Run(name, func(t *testing.T) {
			px := newPublishFixture(t, appAccess)
			exp := px.exported(t, tc.script)
			before := len(px.fake.Tokens())

			res, seen := px.publish(t, px.first(exp))
			require.Empty(t, res.Rejected)
			assert.Equal(t, exp.HeadCommit, px.branchAt(t))

			minted := px.fake.Tokens()[before:]
			require.Len(t, minted, 1)
			assert.Equal(t, tc.perms, minted[0].Permissions)
			require.NotEmpty(t, seen)

			for _, p := range seen {
				assert.Equal(t, minted[0].Value, p)
			}
		})
	}

	t.Run("an App without the permission", func(t *testing.T) {
		px := newPublishFixture(t, appAccess)
		px.fake.Lack("workflows")

		res, seen := px.publish(t, px.first(px.exported(t, workflowChange)))
		assert.Contains(t, res.Rejected, "Workflows")
		assert.Empty(t, seen)
		assert.Empty(t, px.branchAt(t))
		assert.Empty(t, px.fake.Pulls())
	})
}
