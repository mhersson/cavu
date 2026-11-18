package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

const (
	draftQuery    = `query Draft($id: ID!) { node(id: $id) { ... on PullRequest { isDraft } } }`
	readyMutation = `mutation Ready($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { clientMutationId } }`
)

// NewPR is a pull request to open.
type NewPR struct{ Title, Body, Base, Head string } // Head: the branch

// PR is an open pull request.
type PR struct {
	Number      int
	URL, NodeID string
}

// Query names a pull request to observe and what of it a project leaves out
// or trusts.
type Query struct {
	Number                    int
	IgnoredChecks, ReviewBots []string
}

// Observation is a pull request as the forge has it now.
type Observation struct {
	State    string // open, merged, closed
	Draft    bool
	Head     string // the head commit
	Conflict bool
	Checks   string   // none, pending, passed, failed
	Failed   []string // sorted
	Comments []Comment
}

// pull is a pull request as the REST API gives it.
type pull struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	NodeID    string `json:"node_id"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	Mergeable *bool  `json:"mergeable"` // nil while the forge computes it
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p pull) pr() PR { return PR{Number: p.Number, URL: p.HTMLURL, NodeID: p.NodeID} }

// OpenPR returns the open pull request of branch n.Head when the forge has one, else opens a draft.
// A forge that offers no drafts refuses one with 422, and is asked once more for a pull request
// that is ready.
func (c *Client) OpenPR(ctx context.Context, r Repo, a Access, n NewPR) (PR, error) {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return PR{}, err
	}

	q := url.Values{"head": {r.Owner + ":" + n.Head}, "state": {"open"}}

	resp, err := c.rest(ctx, r, token, http.MethodGet, r.path()+"/pulls?"+q.Encode(), nil)
	if err != nil {
		return PR{}, err
	}

	var open []pull
	if err := resp.decode(&open); err != nil {
		return PR{}, err
	}

	if len(open) > 0 {
		return open[0].pr(), nil
	}

	pr, err := c.createPR(ctx, r, token, n, true)
	if he, ok := errors.AsType[*httpError](err); ok && he.status == http.StatusUnprocessableEntity {
		pr, err = c.createPR(ctx, r, token, n, false)
	}

	return pr, err
}

func (c *Client) createPR(ctx context.Context, r Repo, token string, n NewPR, draft bool) (PR, error) {
	body := map[string]any{"title": n.Title, "body": n.Body, "base": n.Base, "head": n.Head}
	if draft {
		body["draft"] = true
	}

	resp, err := c.rest(ctx, r, token, http.MethodPost, r.path()+"/pulls", body)
	if err != nil {
		return PR{}, err
	}

	var p pull
	if err := resp.decode(&p); err != nil {
		return PR{}, err
	}

	return p.pr(), nil
}

// Ready marks a draft ready for review; one that is ready already is no error.
// It asks first, since what the mutation does to a ready one is not documented.
func (c *Client) Ready(ctx context.Context, r Repo, a Access, nodeID string) error {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return err
	}

	vars := map[string]any{"id": nodeID}

	var d struct {
		Node struct {
			IsDraft bool `json:"isDraft"`
		} `json:"node"`
	}

	if err := c.graphQL(ctx, r, token, draftQuery, vars, &d); err != nil {
		return err
	}

	if !d.Node.IsDraft {
		return nil
	}

	return c.graphQL(ctx, r, token, readyMutation, vars, nil)
}

// Comment posts body as a comment on pull request number itself, never in a
// review thread.
func (c *Client) Comment(ctx context.Context, r Repo, a Access, number int, body string) error {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return err
	}

	_, err = c.rest(ctx, r, token, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", r.path(), number), map[string]string{"body": body})

	return err
}

// Observe reads pull request q.Number: its state, its head's checks but those
// q ignores, and the reviewer requests on it, answered or not.
func (c *Client) Observe(ctx context.Context, r Repo, a Access, q Query) (Observation, error) {
	token, err := c.Token(ctx, r, a, ForAPI)
	if err != nil {
		return Observation{}, err
	}

	resp, err := c.rest(ctx, r, token, http.MethodGet, fmt.Sprintf("%s/pulls/%d", r.path(), q.Number), nil)
	if err != nil {
		return Observation{}, err
	}

	var p pull
	if err := resp.decode(&p); err != nil {
		return Observation{}, err
	}

	o := Observation{State: p.State, Draft: p.Draft, Head: p.Head.SHA, Conflict: p.Mergeable != nil && !*p.Mergeable}
	if p.Merged {
		o.State = "merged"
	}

	checks, err := c.checks(ctx, r, token, o.Head, q.IgnoredChecks)
	if err != nil {
		return Observation{}, err
	}

	o.Checks, o.Failed = fold(checks)

	if o.Comments, err = c.comments(ctx, r, token, q); err != nil {
		return Observation{}, err
	}

	return o, nil
}
