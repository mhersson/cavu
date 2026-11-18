//go:build integration

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/mockapi"
)

const (
	// user and password are the user's, set with the verbs.
	user     = "e2e"
	password = "e2e-PASSWORD-0123456789" //nolint:gosec // a test value
	// patience bounds a project's check and a task's run.
	patience = 10 * time.Minute
)

// probe asks for the server's health from the agent's shell: at the address a
// container uses for its host, through the proxy its environment names, and at
// the loopback address the server printed, through the proxy named on the
// line. curl keeps NO_PROXY's loopback off even a named proxy, so --noproxy
// empties that list. E2E_SERVER is the printed address, a project variable.
var probe = mockapi.Step{Tool: "Bash", Input: map[string]any{
	"command": `curl -s --max-time 10 "http://host.docker.internal:${E2E_SERVER##*:}/api/health"; ` +
		`curl -s --max-time 10 --noproxy '' -x "$http_proxy" "http://$E2E_SERVER/api/health"`,
}}

// server is a cavu serve child of an instance as a client reaches it: its
// address, the API token, the session cookie, and every answer it gave. cmd,
// ended and wait are the running child's, as instance.start gives them.
type server struct {
	in      *instance
	url     string
	token   string
	cookie  *http.Cookie
	answers answers
	cmd     *exec.Cmd
	ended   <-chan struct{}
	wait    func() result
}

// answers keeps the bodies of the server's answers as they are read.
type answers struct {
	mu sync.Mutex
	b  strings.Builder
}

func (a *answers) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.b.Write(p)
}

func (a *answers) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.b.String()
}

// transport authorizes each request with auth and keeps the answer's body.
type transport struct {
	s    *server
	auth func(*http.Request)
}

func (tr transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	tr.auth(r)

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &tr.s.answers), resp.Body}
	}

	return resp, err
}

// listening is a server's standard output; it sends the address its
// listening line names.
type listening struct {
	buf  []byte
	addr chan string
}

func (l *listening) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)

	for {
		line, rest, ok := bytes.Cut(l.buf, []byte("\n"))
		if !ok {
			return len(p), nil
		}

		l.buf = rest

		if url, ok := strings.CutPrefix(string(line), "listening on "); ok {
			l.addr <- url
		}
	}
}

// serve makes the user and an API token with the verbs, starts the server on
// a port the kernel picks and logs in. Once the server stopped at the test's
// end, no child's output and no answer may hold a secret.
func (in *instance) serve() *server {
	t := in.t
	t.Helper()

	in.ok("settings", "set", "listen_address", "127.0.0.1:0")

	r := in.cavu(password, "user", "set", user)
	require.Zero(t, r.code, r)

	s := &server{in: in, token: strings.TrimSpace(in.ok("token", "add", "e2e"))}

	t.Cleanup(func() {
		out := in.out.String() + s.answers.String()
		for _, secret := range []string{endpointKey, forgeToken, password} {
			assert.NotContains(t, out, secret)
		}
	})

	s.start()

	_, resp := s.send(s.byPage, http.MethodPost, "/api/auth/login", map[string]string{"name": user, "password": password}, http.StatusNoContent, nil)
	i := slices.IndexFunc(resp.Cookies(), func(c *http.Cookie) bool { return c.Name == "cavu_session" })
	require.GreaterOrEqual(t, i, 0, "the login sets the session cookie")
	s.cookie = resp.Cookies()[i]

	return s
}

// start starts cavu serve and waits for the address it prints. The test's end
// interrupts it, and a cleanup requires it to exit 0 before the instance's
// containers are removed.
func (s *server) start() {
	t := s.in.t
	t.Helper()

	l := &listening{addr: make(chan string, 1)}
	cmd, ended, wait := s.in.start("", l, "serve")
	s.cmd, s.ended, s.wait = cmd, ended, wait

	t.Cleanup(func() {
		r := wait()
		assert.Zero(t, r.code, "cavu serve exits 0 once interrupted")

		if t.Failed() {
			t.Logf("cavu serve:\n%s", r)
		}
	})

	select {
	case s.url = <-l.addr:
	case <-ended:
		t.Fatal("cavu serve ended before it listened")
	case <-time.After(time.Minute):
		t.Fatal("cavu serve printed no address within a minute")
	}
}

// stop interrupts the server, as Ctrl-C does, and requires it to exit 0.
func (s *server) stop() {
	t := s.in.t
	t.Helper()

	require.NoError(t, s.cmd.Process.Signal(os.Interrupt))

	r := s.wait()
	require.Zero(t, r.code, r)
}

// byToken authorizes a request with the API token.
func (s *server) byToken(r *http.Request) { r.Header.Set("Authorization", "Bearer "+s.token) }

// byPage authorizes a request as the web page does: with the session cookie,
// once there is one, and the CSRF header.
func (s *server) byPage(r *http.Request) {
	if s.cookie != nil {
		r.AddCookie(s.cookie)
	}

	r.Header.Set("X-CAVU-CSRF", "1")
}

// send sends method and path to the server, authorized by auth, with body as
// JSON unless it is nil. It requires the status want, decodes the answer into
// out unless out is nil, and returns the answer's body and the answer.
func (s *server) send(auth func(*http.Request), method, path string, body any, want int, out any) (string, *http.Response) {
	t := s.in.t
	t.Helper()

	var content io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)

		content = bytes.NewReader(b)
	}

	r, err := http.NewRequestWithContext(t.Context(), method, s.url+path, content)
	require.NoError(t, err)

	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := (&http.Client{Transport: transport{s, auth}}).Do(r)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, want, resp.StatusCode, "%s %s: %s", method, path, b)

	if out != nil {
		require.NoError(t, json.Unmarshal(b, out), string(b))
	}

	return string(b), resp
}

// call sends a request with the API token, as send does, and returns the
// answer's body.
func (s *server) call(method, path string, body any, want int, out any) string {
	s.in.t.Helper()

	b, _ := s.send(s.byToken, method, path, body, want, out)

	return b
}

// tool calls the MCP tool name with args through the SDK's client with the API
// token, requires an answer and decodes it into out.
func (s *server) tool(name string, args, out any) {
	t := s.in.t
	t.Helper()

	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: s.url + "/mcp", HTTPClient: &http.Client{Transport: transport{s, s.byToken}}, MaxRetries: -1,
	}, nil)
	require.NoError(t, err)

	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError, "%s: %v", name, res.Content)

	b, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, out))
}

// project adds a project on the fixture repository over REST with env and the
// switches at their defaults, asks for its check, waits for the check to end
// and requires its baseline passed. It returns the project's id.
func (s *server) project(name string, env map[string]string) int64 {
	s.in.t.Helper()

	var p struct{ ID int64 }

	s.call(http.MethodPost, "/api/projects", map[string]any{
		"name": name, "repo_url": s.in.repo, "verify_command": verify, "env": env,
	}, http.StatusCreated, &p)
	s.call(http.MethodPost, fmt.Sprintf("/api/projects/%d/check", p.ID), nil, http.StatusAccepted, nil)

	var got struct {
		Runnable bool
		Check    struct {
			RequestedAt *time.Time `json:"requested_at"`
		}
	}

	within(s.in.t, patience, func() (bool, string) {
		b := s.call(http.MethodGet, fmt.Sprintf("/api/projects/%d", p.ID), nil, http.StatusOK, &got)

		return got.Check.RequestedAt == nil, b
	})
	require.True(s.in.t, got.Runnable, "the check passed the baseline")

	return p.ID
}

// apiTask is what the tests read of a task as the API gives it; a null stop
// is the zero stop.
type apiTask struct {
	ID        int64
	Status    string
	Requester string
	Stop      struct{ Reason, Explanation string }
}

// await polls the task over REST until its status is one of statuses, and
// returns it.
func (s *server) await(id int64, statuses ...string) apiTask {
	s.in.t.Helper()

	var got apiTask

	within(s.in.t, patience, func() (bool, string) {
		got = apiTask{}
		b := s.call(http.MethodGet, fmt.Sprintf("/api/tasks/%d", id), nil, http.StatusOK, &got)

		return slices.Contains(statuses, got.Status), b
	})

	return got
}

// events pages through the task's events over REST, oldest first.
func (s *server) events(id int64) []apiEvent {
	s.in.t.Helper()

	var all []apiEvent

	for {
		var after int64
		if len(all) > 0 {
			after = all[len(all)-1].ID
		}

		var page struct{ Events []apiEvent }

		s.call(http.MethodGet, fmt.Sprintf("/api/tasks/%d/events?after=%d", id, after), nil, http.StatusOK, &page)

		if len(page.Events) == 0 {
			return all
		}

		all = append(all, page.Events...)
	}
}

// within calls ok every half second until it reports true, and fails the test
// with what it last reported once bound passed.
func within(t *testing.T, bound time.Duration, ok func() (bool, string)) {
	t.Helper()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	limit := time.After(bound)

	for {
		done, state := ok()
		if done {
			return
		}

		select {
		case <-limit:
			t.Fatalf("not within %s:\n%s", bound, state)
		case <-tick.C:
		}
	}
}

func TestServeVerbsBesideTheServer(t *testing.T) {
	// The readiness turn, then the work that resumes its session, then a review.
	mock := mockapi.Start(t, []mockapi.Step{ready, change, done, readDiff, passed})
	in := setup(t, mock.HostURL())
	s := in.serve()

	// The doctor drives containers, so it refuses before it checks anything.
	r := in.cavu("", "doctor")
	assert.Equal(t, 1, r.code, r)
	assert.Empty(t, r.stdout)
	assert.Contains(t, r.stderr, in.dir)

	// The server runs the check, and the verb exits 0 once it passed the baseline.
	in.ok("project", "add", "notes", "--repo", in.repo, "--verify", verify)
	in.ok("project", "check", "notes")

	id := in.task("notes")
	in.ok("task", "run", id)

	var got apiTask

	s.call(http.MethodGet, "/api/tasks/"+id, nil, http.StatusOK, &got)
	assert.Equal(t, "in_review", got.Status, "%+v", got)
	assert.Equal(t, "cli", got.Requester)
}

func TestSandboxCannotReachTheServer(t *testing.T) {
	// The readiness turn, then the work that probes the server first, then a review.
	mock := mockapi.Start(t, []mockapi.Step{ready, probe, change, done, readDiff, passed})
	in := setup(t, mock.HostURL())
	s := in.serve()

	addr := strings.TrimPrefix(s.url, "http://")
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	s.project("notes", map[string]string{"E2E_SERVER": addr})

	var task apiTask

	s.tool("create_task", map[string]any{"project": "notes", "title": title, "start": true}, &task)
	task = s.await(task.ID, "in_review", "blocked")
	require.Equal(t, "in_review", task.Status, "%+v", task)

	var hosts []string
	for _, d := range payloads[struct{ Host string }](t, s.events(task.ID), "egress_denied") {
		hosts = append(hosts, d.Host)
	}

	assert.Contains(t, hosts, "host.docker.internal:"+port)
	assert.Contains(t, hosts, addr)
}

func TestServeCancelInterruptsSession(t *testing.T) {
	// The readiness turn's model call is held and never answered, so only a
	// cut ends its session.
	mock := mockapi.Start(t, nil)
	in := setup(t, mock.HostURL())
	s := in.serve()
	project := s.project("notes", nil)

	mock.Hold()

	var task apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)
	require.True(t, modelCalled(mock, s.ended), "cavu serve made no model call")

	s.send(s.byPage, http.MethodPost, fmt.Sprintf("/api/tasks/%d/actions", task.ID), map[string]string{"action": "cancel"}, http.StatusOK, &task)
	assert.Equal(t, "cancelled", task.Status)

	// A cancel removes the run's containers, its workspace and hand-off and
	// the task's home; the project's cache and mirror stay.
	prefix, p := "cavu-"+in.id[:8]+"-", strconv.FormatInt(project, 10)
	kept := []string{prefix + "cache-" + p, prefix + "mirror-" + p}

	within(t, 30*time.Second, func() (bool, string) {
		containers, volumes, err := in.rt.List(t.Context(), "cavu.instance="+in.id)
		require.NoError(t, err)

		return len(containers) == 0 && slices.Equal(volumes, kept), fmt.Sprintf("containers %v\nvolumes %v", containers, volumes)
	})

	var got apiTask

	s.call(http.MethodGet, fmt.Sprintf("/api/tasks/%d", task.ID), nil, http.StatusOK, &got)
	assert.Equal(t, "cancelled", got.Stop.Reason, "%+v", got)
}

func TestServeRestartContinues(t *testing.T) {
	// The held readiness call takes the first step, which the cut session never receives.
	mock := mockapi.Start(t, []mockapi.Step{ready, change, done, readDiff, passed})
	in := setup(t, mock.HostURL())
	s := in.serve()
	s.project("notes", nil)

	mock.Hold()

	var task apiTask

	s.call(http.MethodPost, "/api/tasks", map[string]any{"project": "notes", "title": title, "start": true}, http.StatusCreated, &task)
	require.True(t, modelCalled(mock, s.ended), "cavu serve made no model call")

	// The second server's containers carry the first's names, so the first's are told apart by id.
	ids := func(filter string) []string {
		return strings.Fields(in.client("ps", "--all", "--quiet", "--no-trunc", "--filter="+filter))
	}
	first := ids("label=cavu.instance=" + in.id)
	require.NotEmpty(t, first)

	s.stop()
	s.start()

	task = s.await(task.ID, "in_review", "blocked")
	require.Equal(t, "in_review", task.Status, "%+v", task)
	assert.Equal(t, 1, in.runs(strconv.FormatInt(task.ID, 10)))

	// The cut readiness turn is continued in its own session.
	assert.Equal(t, []sessionStart{{Mode: "readiness"}, {Mode: "continue", Resume: true}}, sessionStarts(t, s.events(task.ID)))

	within(t, time.Minute, func() (bool, string) {
		left := in.containers()

		return len(left) == 0, fmt.Sprintf("containers %v", left)
	})

	for _, id := range first {
		assert.Empty(t, ids("id="+id), "a container of the first server is left")
	}
}
