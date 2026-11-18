// Package forgefake serves a small stateful GitHub for tests: the REST and
// GraphQL routes CAVU's forge client calls, under /api/v3 and /api/graphql as
// on GitHub Enterprise. It runs the host's git and is never part of the
// shipped binary.
package forgefake

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/cavu/internal/testkit/gitrepo"
)

const graphQLPath = "/api/graphql"

// Pull is a pull request the fake holds.
type Pull struct {
	Number      int
	NodeID      string
	Title, Body string
	Base, Head  string // branches
	Draft       bool
	State       string // open or closed
	Merged      bool
}

// CheckRun is a check run of a commit.
type CheckRun struct {
	ID             int64  // its job's id when App is github-actions
	App            string // the app's slug
	Name           string
	Status         string // queued, in_progress or completed
	Conclusion     string // once completed
	Title, Summary string // its output
	URL            string
}

// Status is a commit status.
type Status struct{ Context, State, URL string }

// Request is one request the fake was sent.
type Request struct {
	Method    string
	Path      string // with its query
	Operation string // a GraphQL document's operation name
	Header    http.Header
}

// Token is an App installation token the fake minted.
type Token struct {
	Value       string
	Permissions map[string]string
}

// Fake is a GitHub holding one repository, the bare one it was given. It is an
// http.Handler and safe for concurrent use.
type Fake struct {
	t    *testing.T
	bare string
	mux  *http.ServeMux

	mu       sync.Mutex
	ids      int
	pulls    []*pull
	runs     map[string][]CheckRun
	statuses map[string][]Status
	logs     map[int64]string
	perms    map[string]string
	noDrafts bool
	fail     int
	limit    time.Time
	requests []Request
	tokens   []Token
}

type pull struct {
	Pull
	mergeable *bool // nil until the forge has computed it
	reviews   []review
	threads   []*Thread
}

type review struct{ id, author, state, body string }

type comment struct {
	id, author, body string
	pending          bool
}

// Thread is a review thread on a pull request.
type Thread struct {
	ID string // its first comment's id

	f        *Fake
	path     string
	line     int // 0 when the line is gone from the head
	original int
	resolved bool
	comments []comment
}

// New returns a fake whose pull requests' heads are their branches in the
// bare repository at bareDir.
func New(t *testing.T, bareDir string) *Fake {
	t.Helper()

	f := &Fake{
		t: t, bare: bareDir, mux: http.NewServeMux(),
		runs: map[string][]CheckRun{}, statuses: map[string][]Status{}, logs: map[int64]string{}, perms: map[string]string{},
	}

	const repo = "/api/v3/repos/{o}/{n}"

	f.mux.HandleFunc("GET "+repo+"/installation", f.installation)
	f.mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", f.accessToken)
	f.mux.HandleFunc("GET "+repo+"/pulls", f.listPulls)
	f.mux.HandleFunc("POST "+repo+"/pulls", f.createPull)
	f.mux.HandleFunc("GET "+repo+"/pulls/{number}", f.getPull)
	f.mux.HandleFunc("GET "+repo+"/commits/{sha}/check-runs", f.checkRuns)
	f.mux.HandleFunc("GET "+repo+"/commits/{sha}/status", f.status)
	f.mux.HandleFunc("GET "+repo+"/actions/jobs/{id}/logs", f.jobLog)
	f.mux.HandleFunc("GET "+repo+"/collaborators/{login}/permission", f.permission)

	return f
}

// ServeHTTP records the request, then answers 502 while Fail's count lasts,
// as rate limited while Limit holds, and else as GitHub does.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var gql graphQLRequest

	if r.URL.Path == graphQLPath {
		_ = json.NewDecoder(r.Body).Decode(&gql)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, Request{
		Method: r.Method, Path: r.URL.RequestURI(), Operation: gql.operation(), Header: r.Header.Clone(),
	})

	switch {
	case f.fail > 0:
		f.fail--

		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	case !f.limit.IsZero():
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.limit.Unix(), 10))

		if r.URL.Path == graphQLPath {
			graphQLError(w, "RATE_LIMITED", "API rate limit exceeded")

			return
		}

		writeJSON(w, http.StatusForbidden, message("API rate limit exceeded"))
	case r.URL.Path == graphQLPath && r.Method == http.MethodPost:
		f.graphQL(w, gql)
	default:
		f.mux.ServeHTTP(w, r)
	}
}

// SetChecks sets the check runs of commit sha.
func (f *Fake) SetChecks(sha string, runs ...CheckRun) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.runs[sha] = runs
}

// SetStatus sets the commit statuses of commit sha.
func (f *Fake) SetStatus(sha string, statuses ...Status) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.statuses[sha] = statuses
}

// SetJobLog sets the log of Actions job id; an empty log answers 416, as
// GitHub does for a log it holds no bytes of.
func (f *Fake) SetJobLog(id int64, log string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.logs[id] = log
}

// SetPermission sets login's permission on the repository: admin, write, read
// or none. A login never set is no collaborator.
func (f *Fake) SetPermission(login, permission string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.perms[login] = permission
}

// Review adds a review of state (APPROVED, CHANGES_REQUESTED, DISMISSED,
// COMMENTED) to pull request number and returns its id. An empty author is a
// deleted account.
func (f *Fake) Review(number int, author, state, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	p := f.mustPull(number)
	id := f.newID("PRR")
	p.reviews = append(p.reviews, review{id: id, author: author, state: state, body: body})

	return id
}

// Thread starts a review thread on line of path in pull request number.
func (f *Fake) Thread(number int, author, path string, line int, body string) *Thread {
	f.mu.Lock()
	defer f.mu.Unlock()

	p := f.mustPull(number)
	th := &Thread{ID: f.newID("PRRC"), f: f, path: path, line: line, original: line}
	th.comments = []comment{{id: th.ID, author: author, body: body}}
	p.threads = append(p.threads, th)

	return th
}

// Reply adds a comment to the thread and returns its id.
func (th *Thread) Reply(author, body string) string {
	th.f.mu.Lock()
	defer th.f.mu.Unlock()

	id := th.f.newID("PRRC")
	th.comments = append(th.comments, comment{id: id, author: author, body: body})

	return id
}

// Resolve marks the thread resolved.
func (th *Thread) Resolve() {
	th.f.mu.Lock()
	defer th.f.mu.Unlock()

	th.resolved = true
}

// MarkPending makes the thread's newest comment part of a review its author
// has not submitted yet.
func (th *Thread) MarkPending() {
	th.f.mu.Lock()
	defer th.f.mu.Unlock()

	th.comments[len(th.comments)-1].pending = true
}

// Move sets the line the thread refers to on the head; 0 is a line gone from
// it, as on an outdated thread.
func (th *Thread) Move(line int) {
	th.f.mu.Lock()
	defer th.f.mu.Unlock()

	th.line = line
}

// Merge merges pull request number.
func (f *Fake) Merge(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p := f.mustPull(number)
	p.State, p.Merged = "closed", true
}

// Close closes pull request number without a merge.
func (f *Fake) Close(number int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mustPull(number).State = "closed"
}

// Conflict sets whether pull request number conflicts with its base. Until it
// is called the forge has not computed it, as for a new pull request.
func (f *Fake) Conflict(number int, conflict bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	mergeable := !conflict
	f.mustPull(number).mergeable = &mergeable
}

// NoDrafts makes the fake a forge that offers no drafts: it answers 422 to a
// draft, as GitHub does for a private repository on the free plan.
func (f *Fake) NoDrafts() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.noDrafts = true
}

// Fail makes the next n requests answer 502.
func (f *Fake) Fail(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fail = n
}

// Limit answers every request as rate limited until the next call with the
// zero time lifts it; until is the reset time the answers name.
func (f *Fake) Limit(until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.limit = until
}

// Pulls returns the pull requests in the order they were opened.
func (f *Fake) Pulls() []Pull {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Pull, len(f.pulls))
	for i, p := range f.pulls {
		out[i] = p.Pull
	}

	return out
}

// Requests returns the requests the fake was sent, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.requests)
}

// Tokens returns the App tokens the fake minted, in order.
func (f *Fake) Tokens() []Token {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.tokens)
}

func (f *Fake) newID(prefix string) string {
	f.ids++

	return prefix + "_" + strconv.Itoa(f.ids)
}

func (f *Fake) mustPull(number int) *pull {
	p := f.findPull(func(p *pull) bool { return p.Number == number })
	if p == nil {
		f.t.Fatalf("forgefake: no pull request %d", number)
	}

	return p
}

func (f *Fake) findPull(match func(*pull) bool) *pull {
	i := slices.IndexFunc(f.pulls, match)
	if i < 0 {
		return nil
	}

	return f.pulls[i]
}

// branchHead is branch's commit in the bare repository, "" when it has no such
// branch: for-each-ref exits 0 then, so no handler fails the test.
func (f *Fake) branchHead(branch string) string {
	return gitrepo.Git(f.t, f.bare, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
}

func (f *Fake) pullJSON(r *http.Request, p *pull) map[string]any {
	return map[string]any{
		"number":    p.Number,
		"node_id":   p.NodeID,
		"html_url":  fmt.Sprintf("https://%s/%s/%s/pull/%d", r.Host, r.PathValue("o"), r.PathValue("n"), p.Number),
		"title":     p.Title,
		"body":      p.Body,
		"state":     p.State,
		"draft":     p.Draft,
		"merged":    p.Merged,
		"mergeable": p.mergeable,
		"head":      map[string]any{"ref": p.Head, "sha": f.branchHead(p.Head)},
		"base":      map[string]any{"ref": p.Base},
	}
}

func (f *Fake) installation(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"id": 1})
}

func (f *Fake) accessToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Permissions map[string]string `json:"permissions"`
	}

	_ = json.NewDecoder(r.Body).Decode(&in)

	tok := Token{Value: "ghs_fake" + strconv.Itoa(len(f.tokens)+1), Permissions: in.Permissions}
	f.tokens = append(f.tokens, tok)

	writeJSON(w, http.StatusCreated, map[string]any{"token": tok.Value, "expires_at": time.Now().Add(time.Hour)})
}

func (f *Fake) listPulls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := cmp.Or(q.Get("state"), "open")
	_, branch, _ := strings.Cut(q.Get("head"), ":")

	out := []map[string]any{}

	for _, p := range f.pulls {
		if (branch == "" || p.Head == branch) && (state == "all" || p.State == state) {
			out = append(out, f.pullJSON(r, p))
		}
	}

	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) createPull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title, Body, Base, Head string
		Draft                   bool
	}

	_ = json.NewDecoder(r.Body).Decode(&in)

	if in.Draft && f.noDrafts || f.branchHead(in.Head) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, message("Validation Failed"))

		return
	}

	n := len(f.pulls) + 1
	p := &pull{
		Number: n, NodeID: "PR_" + strconv.Itoa(n), Title: in.Title, Body: in.Body, Base: in.Base, Head: in.Head,
		Draft: in.Draft, State: "open",
	}
	f.pulls = append(f.pulls, p)

	writeJSON(w, http.StatusCreated, f.pullJSON(r, p))
}

func (f *Fake) getPull(w http.ResponseWriter, r *http.Request) {
	p := f.findPull(func(p *pull) bool { return strconv.Itoa(p.Number) == r.PathValue("number") })
	if p == nil {
		writeJSON(w, http.StatusNotFound, message("Not Found"))

		return
	}

	writeJSON(w, http.StatusOK, f.pullJSON(r, p))
}

func (f *Fake) checkRuns(w http.ResponseWriter, r *http.Request) {
	runs := f.runs[r.PathValue("sha")]
	out := make([]map[string]any, len(runs))

	for i, c := range runs {
		var conclusion any
		if c.Conclusion != "" {
			conclusion = c.Conclusion
		}

		out[i] = map[string]any{
			"id": c.ID, "app": map[string]any{"slug": c.App}, "name": c.Name, "status": c.Status,
			"conclusion": conclusion, "html_url": c.URL, "output": map[string]any{"title": c.Title, "summary": c.Summary},
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "check_runs": out})
}

func (f *Fake) status(w http.ResponseWriter, r *http.Request) {
	statuses := f.statuses[r.PathValue("sha")]
	out := make([]map[string]any, len(statuses))

	for i, s := range statuses {
		out[i] = map[string]any{"context": s.Context, "state": s.State, "target_url": s.URL}
	}

	writeJSON(w, http.StatusOK, map[string]any{"statuses": out})
}

func (f *Fake) jobLog(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)

	log, ok := f.logs[id]

	switch {
	case !ok:
		writeJSON(w, http.StatusNotFound, message("Not Found"))
	case log == "":
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	default:
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(log))
	}
}

func (f *Fake) permission(w http.ResponseWriter, r *http.Request) {
	p, ok := f.perms[r.PathValue("login")]
	if !ok {
		writeJSON(w, http.StatusNotFound, message("Not Found"))

		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"permission": p})
}

type graphQLRequest struct {
	Query     string `json:"query"`
	Variables struct {
		Owner  string `json:"owner"`
		Name   string `json:"name"`
		Number int    `json:"number"`
		ID     string `json:"id"`
	} `json:"variables"`
}

var operationName = regexp.MustCompile(`^\s*(?:query|mutation)\s+(\w+)`)

func (g graphQLRequest) operation() string {
	if m := operationName.FindStringSubmatch(g.Query); m != nil {
		return m[1]
	}

	return ""
}

// graphQL answers the client's three documents by their operation names.
func (f *Fake) graphQL(w http.ResponseWriter, g graphQLRequest) {
	switch op := g.operation(); op {
	case "Observe":
		p := f.findPull(func(p *pull) bool { return p.Number == g.Variables.Number })
		if p == nil {
			graphQLError(w, "NOT_FOUND", "Could not resolve to a PullRequest")

			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"repository": map[string]any{"pullRequest": f.reviewsJSON(p)},
		}})
	case "Draft", "Ready":
		p := f.findPull(func(p *pull) bool { return p.NodeID == g.Variables.ID })
		if p == nil {
			graphQLError(w, "NOT_FOUND", "Could not resolve to a node")

			return
		}

		if op == "Draft" {
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"node": map[string]any{"isDraft": p.Draft}}})

			return
		}

		p.Draft = false

		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"markPullRequestReadyForReview": map[string]any{"clientMutationId": nil},
		}})
	default:
		graphQLError(w, "", "forgefake: unknown operation "+strconv.Quote(op))
	}
}

// reviewsJSON is a pull request's latest opinion per author, a review that
// approves, requests changes or was dismissed, and its review threads.
func (f *Fake) reviewsJSON(p *pull) map[string]any {
	var reviews []map[string]any

	latest := map[string]int{}

	for _, rv := range p.reviews {
		if rv.state != "APPROVED" && rv.state != "CHANGES_REQUESTED" && rv.state != "DISMISSED" {
			continue
		}

		node := map[string]any{"id": rv.id, "state": rv.state, "body": rv.body, "author": actor(rv.author)}

		if i, ok := latest[rv.author]; ok {
			reviews[i] = node

			continue
		}

		latest[rv.author] = len(reviews)
		reviews = append(reviews, node)
	}

	threads := make([]map[string]any, len(p.threads))

	for i, th := range p.threads {
		comments := make([]map[string]any, len(th.comments))
		for j, c := range th.comments {
			state := "SUBMITTED"
			if c.pending {
				state = "PENDING"
			}

			comments[j] = map[string]any{"id": c.id, "state": state, "body": c.body, "author": actor(c.author)}
		}

		var line any
		if th.line > 0 {
			line = th.line
		}

		threads[i] = map[string]any{
			"isResolved": th.resolved, "path": th.path, "line": line, "originalLine": th.original,
			"comments": map[string]any{"nodes": comments},
		}
	}

	return map[string]any{
		"latestOpinionatedReviews": map[string]any{"nodes": reviews},
		"reviewThreads":            map[string]any{"nodes": threads},
	}
}

// actor is an author as GraphQL gives it: null for a deleted account, and for
// a login that ends in [bot] a Bot whose login lacks the [bot] the REST API
// shows.
func actor(login string) any {
	if bot, ok := strings.CutSuffix(login, "[bot]"); ok {
		return map[string]any{"__typename": "Bot", "login": bot}
	}

	if login == "" {
		return nil
	}

	return map[string]any{"__typename": "User", "login": login}
}

func graphQLError(w http.ResponseWriter, typ, msg string) {
	writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]any{{"type": typ, "message": msg}}})
}

func message(msg string) map[string]string { return map[string]string{"message": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
