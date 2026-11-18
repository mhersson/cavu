package forge

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// maxCheckPages bounds the pages of check runs one read takes.
const maxCheckPages = 5

const (
	checkPending = "pending"
	checkPassed  = "passed"
	checkFailed  = "failed"
)

// check is one check run or commit status of a commit.
type check struct {
	id             int64  // a run's id, which is its job's for an Actions job
	app            string // a run's app slug; "" for a status
	name, url      string
	title, summary string // a run's output
	state          string // pending, passed or failed
	conclusion     string // a run's, or a status's state
}

// checks reads the check runs of commit sha over at most five pages, then its
// statuses, and leaves out those named in ignored.
func (c *Client) checks(ctx context.Context, r Repo, token, sha string, ignored []string) ([]check, error) {
	var out []check

	next := r.restURL() + r.path() + "/commits/" + sha + "/check-runs?per_page=100"

	for page := 0; page < maxCheckPages && next != ""; page++ {
		var body struct {
			CheckRuns []struct {
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
			} `json:"check_runs"`
		}

		var err error
		if next, err = c.page(ctx, token, next, &body); err != nil {
			return nil, err
		}

		for _, run := range body.CheckRuns {
			out = append(out, check{
				id: run.ID, app: run.App.Slug, name: run.Name, url: cmp.Or(run.HTMLURL, run.DetailsURL),
				title: run.Output.Title, summary: run.Output.Summary, state: runState(run.Status, run.Conclusion),
				conclusion: run.Conclusion,
			})
		}
	}

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

	for _, s := range combined.Statuses {
		out = append(out, check{name: s.Context, url: s.TargetURL, state: statusState(s.State), conclusion: s.State})
	}

	return slices.DeleteFunc(out, func(ch check) bool { return slices.Contains(ignored, ch.name) }), nil
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
