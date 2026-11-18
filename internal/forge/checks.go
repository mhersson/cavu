package forge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// maxCheckPages bounds the pages of each list one read of checks takes.
const maxCheckPages = 5

const (
	checkPending = "pending"
	checkPassed  = "passed"
	checkFailed  = "failed"
)

// check is one check run, Actions job or commit status of a commit.
type check struct {
	id             int64  // a run's id, which is its job's for an Actions job
	app            string // a run's app slug, github-actions for a job; "" for a status
	name, url      string
	title, summary string // a run's output
	state          string // pending, passed or failed
	conclusion     string // a run's, or a status's state
}

// listPage is a page of a commit's check runs, of its workflow runs or of a
// run's jobs.
type listPage struct {
	CheckRuns    []checkRun `json:"check_runs"`
	WorkflowRuns []struct {
		ID int64 `json:"id"`
	} `json:"workflow_runs"`
	Jobs []checkRun `json:"jobs"`
}

// checkRun is a check run, or an Actions job: a job has its check run's id,
// name, status, conclusion and address, and no app or output.
type checkRun struct {
	ID  int64 `json:"id"`
	App struct {
		Slug string `json:"slug"`
	} `json:"app"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

func (cr checkRun) check(app string) check {
	return check{
		id: cr.ID, app: app, name: cr.Name, url: cmp.Or(cr.HTMLURL, cr.DetailsURL),
		title: cr.Output.Title, summary: cr.Output.Summary, state: runState(cr.Status, cr.Conclusion),
		conclusion: cr.Conclusion,
	}
}

// credential is a token on a repository, the token held only as its hash.
type credential struct {
	repo  Repo
	token [sha256.Size]byte
}

// ReadChecks reads commit sha's checks as Observe does and reports whether it
// read them as the jobs of its workflow runs, GitHub having refused the
// credential the Checks API.
func (c *Client) ReadChecks(ctx context.Context, r Repo, a Access, sha string) (bool, error) {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return false, err
	}

	if _, err := c.checks(ctx, r, token, sha, nil); err != nil {
		return false, err
	}

	return c.readsJobs(r, token), nil
}

// checks reads the checks of commit sha, then its statuses, and leaves out
// those named in ignored. Its checks are its check runs or, once GitHub
// refused token the Checks API on r, the jobs of its workflow runs.
func (c *Client) checks(ctx context.Context, r Repo, token, sha string, ignored []string) ([]check, error) {
	jobs := c.readsJobs(r, token)

	var (
		out []check
		err error
	)

	if !jobs {
		out, err = c.checkRuns(ctx, r, token, sha)
		if jobs = refusesChecks(err); jobs {
			c.refuseChecks(r, token)
		}
	}

	if jobs {
		out, err = c.jobs(ctx, r, token, sha)
	}

	if err != nil {
		return nil, refusedBoth(jobs, err)
	}

	statuses, err := c.statuses(ctx, r, token, sha)
	if err != nil {
		return nil, refusedBoth(jobs, err)
	}

	out = append(out, statuses...)

	return slices.DeleteFunc(out, func(ch check) bool { return slices.Contains(ignored, ch.name) }), nil
}

// readsJobs reports whether the checks of r are read with token as Actions
// jobs, since GitHub refused it the Checks API.
func (c *Client) readsJobs(r Repo, token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.noChecks[credential{r, sha256.Sum256([]byte(token))}]
}

func (c *Client) refuseChecks(r Repo, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.noChecks[credential{r, sha256.Sum256([]byte(token))}] = true
}

// refusesChecks reports whether err is GitHub's refusal of the check runs to a
// personal access token: GitHub documents no fine-grained permission for them.
// It is strict because the refusal is remembered: one worded otherwise must
// not hide other apps' check runs until a restart.
func refusesChecks(err error) bool {
	he, ok := errors.AsType[*httpError](err)

	return ok && he.status == http.StatusForbidden &&
		strings.Contains(strings.ToLower(he.message), "resource not accessible by personal access token")
}

// refusedBoth is err, but for GitHub's refusal of a read of the jobs or the
// statuses it names what each kind of credential needs.
func refusedBoth(jobs bool, err error) error {
	he, ok := errors.AsType[*httpError](err)
	if !jobs || !ok || (he.status != http.StatusForbidden && he.status != http.StatusNotFound) {
		return err
	}

	return fmt.Errorf("GitHub refused to show the commit's checks: a personal access token needs Actions and "+
		"Commit statuses read, a GitHub App Checks, Actions and Commit statuses read: %w", err)
}

func (c *Client) checkRuns(ctx context.Context, r Repo, token, sha string) ([]check, error) {
	var out []check

	err := c.pages(ctx, token, r.restURL()+r.path()+"/commits/"+sha+"/check-runs?per_page=100", func(p *listPage) {
		for _, run := range p.CheckRuns {
			out = append(out, run.check(run.App.Slug))
		}
	})

	return out, err
}

// jobs reads the jobs of commit sha's workflow runs. GitHub lists a run's
// newest attempt by default, as it does check runs, so a re-run's earlier
// failure is left out both ways.
func (c *Client) jobs(ctx context.Context, r Repo, token, sha string) ([]check, error) {
	runs := r.restURL() + r.path() + "/actions/runs"

	var ids []int64

	err := c.pages(ctx, token, runs+"?head_sha="+sha+"&per_page=100", func(p *listPage) {
		for _, run := range p.WorkflowRuns {
			ids = append(ids, run.ID)
		}
	})
	if err != nil {
		return nil, err
	}

	var out []check

	for _, id := range ids {
		err := c.pages(ctx, token, fmt.Sprintf("%s/%d/jobs?per_page=100", runs, id), func(p *listPage) {
			for _, job := range p.Jobs {
				out = append(out, job.check("github-actions"))
			}
		})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (c *Client) statuses(ctx context.Context, r Repo, token, sha string) ([]check, error) {
	resp, err := c.rest(ctx, r, token, http.MethodGet, r.path()+"/commits/"+sha+"/status?per_page=100", nil)
	if err != nil {
		return nil, err
	}

	var combined struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}

	if err := resp.decode(&combined); err != nil {
		return nil, err
	}

	out := make([]check, len(combined.Statuses))
	for i, s := range combined.Statuses {
		out[i] = check{name: s.Context, url: s.TargetURL, state: statusState(s.State), conclusion: s.State}
	}

	return out, nil
}

// pages reads a list from addr over at most five pages, handing each to add.
func (c *Client) pages(ctx context.Context, token, addr string, add func(*listPage)) error {
	for page := 0; page < maxCheckPages && addr != ""; page++ {
		var p listPage

		var err error
		if addr, err = c.page(ctx, token, addr, &p); err != nil {
			return err
		}

		add(&p)
	}

	return nil
}

// page GETs addr into v and returns the next page's address, "" after the
// last. A next page on another origin is an error, so the token goes nowhere
// else.
func (c *Client) page(ctx context.Context, token, addr string, v any) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, addr, token, nil)
	if err != nil {
		return "", err
	}

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}

	if err := resp.decode(v); err != nil {
		return "", err
	}

	next := nextLink(resp.header.Get("Link"))
	if next == "" {
		return "", nil
	}

	if u, err := url.Parse(next); err != nil || !sameOrigin(u, req.URL) {
		return "", fmt.Errorf("%s: the next page's link points to another origin", resp.op)
	}

	return next, nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// nextLink returns the rel="next" target of a Link header, or "".
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}

		target = strings.TrimSpace(target)
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}

		for p := range strings.SplitSeq(params, ";") {
			if strings.ReplaceAll(strings.TrimSpace(p), " ", "") == `rel="next"` {
				return target[1 : len(target)-1]
			}
		}
	}

	return ""
}

func runState(status, conclusion string) string {
	if status != "completed" {
		return checkPending
	}

	switch conclusion {
	case "success", "neutral", "skipped":
		return checkPassed
	default: // an unknown conclusion must not read as green
		return checkFailed
	}
}

func statusState(state string) string {
	switch state {
	case "success":
		return checkPassed
	case "pending":
		return checkPending
	default: // error, failure, or an unknown state
		return checkFailed
	}
}

// fold is a head's checks in one word, none, pending, passed or failed, and
// the names of the failed ones, sorted.
func fold(checks []check) (string, []string) {
	var failed []string

	pending := false

	for _, ch := range checks {
		switch ch.state {
		case checkPending:
			pending = true
		case checkFailed:
			failed = append(failed, ch.name)
		}
	}

	slices.Sort(failed)
	failed = slices.Compact(failed)

	switch {
	case len(checks) == 0:
		return "none", nil
	case pending:
		return checkPending, failed
	case len(failed) > 0:
		return checkFailed, failed
	default:
		return checkPassed, nil
	}
}
