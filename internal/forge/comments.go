package forge

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

// bodyCap bounds each body a reviewer request carries.
const bodyCap = 4 << 10

const observeQuery = `query Observe($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      latestOpinionatedReviews(last: 100) { nodes { id state body author { login } } }
      reviewThreads(last: 100) {
        nodes {
          isResolved path line originalLine
          comments(last: 100) { nodes { id body author { login } } }
        }
      }
    }
  }
}`

// Comment is a reviewer's request on a pull request: a review, or a thread on
// a line of a file.
type Comment struct {
	ID, Author, Path string
	Line             int
	Body             string
}

// author is a review's or a comment's; a deleted account's is null, so its
// login is "".
type author struct {
	Login string `json:"login"`
}

type pullRequestNode struct {
	LatestOpinionatedReviews struct {
		Nodes []struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Body   string `json:"body"`
			Author author `json:"author"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved   bool   `json:"isResolved"`
			Path         string `json:"path"`
			Line         int    `json:"line"` // 0 when the line is gone from the head
			OriginalLine int    `json:"originalLine"`
			Comments     struct {
				Nodes []struct {
					ID     string `json:"id"`
					Body   string `json:"body"`
					Author author `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

// comments reads the reviewer requests on pull request q.Number. An author is
// allowed when q.ReviewBots names the login, else when the forge says the
// account may write; each other login is asked once.
func (c *Client) comments(ctx context.Context, r Repo, token string, q Query) ([]Comment, error) {
	var d struct {
		Repository struct {
			PullRequest pullRequestNode `json:"pullRequest"`
		} `json:"repository"`
	}

	vars := map[string]any{"owner": r.Owner, "name": r.Name, "number": q.Number}
	if err := c.graphQL(ctx, r, token, observeQuery, vars, &d); err != nil {
		return nil, err
	}

	known := map[string]bool{}
	for _, b := range q.ReviewBots {
		known[loginKey(b)] = true
	}

	known[""] = false // a deleted account

	allowed := func(login string) (bool, error) {
		k := loginKey(login)
		if ok, seen := known[k]; seen {
			return ok, nil
		}

		ok, err := c.writer(ctx, r, token, login)
		known[k] = ok

		return ok, err
	}

	return requests(d.Repository.PullRequest, allowed)
}

// loginKey compares logins: GitHub's are case-insensitive, and GraphQL gives a
// bot's without the [bot] the REST API and GitHub's page show.
func loginKey(login string) string { return strings.TrimSuffix(strings.ToLower(login), "[bot]") }

// writer reports whether login's permission on r is admin or write; maintain
// reads as write. An account that is no collaborator answers 404.
func (c *Client) writer(ctx context.Context, r Repo, token, login string) (bool, error) {
	resp, err := c.rest(ctx, r, token, http.MethodGet, r.path()+"/collaborators/"+login+"/permission", nil)
	if he, ok := errors.AsType[*httpError](err); ok && he.status == http.StatusNotFound {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	var p struct {
		Permission string `json:"permission"`
	}

	if err := resp.decode(&p); err != nil {
		return false, err
	}

	return p.Permission == "admin" || p.Permission == "write", nil
}

// requests are the reviewer requests on a pull request, answered or not. A
// review counts when it requests changes, has a body and its author is
// allowed. A thread counts when it is not resolved and holds a comment by an
// allowed author: it is one request with the id and author of its newest such
// comment, its line on the head else its original line, and the allowed
// comments' bodies. Neither is held to a commit, so an outdated thread counts.
func requests(pr pullRequestNode, allowed func(login string) (bool, error)) ([]Comment, error) {
	var out []Comment

	for _, rv := range pr.LatestOpinionatedReviews.Nodes {
		if rv.State != "CHANGES_REQUESTED" || rv.Body == "" {
			continue
		}

		ok, err := allowed(rv.Author.Login)
		if err != nil {
			return nil, err
		}

		if ok {
			out = append(out, Comment{ID: rv.ID, Author: rv.Author.Login, Body: cut(rv.Body, bodyCap)})
		}
	}

	for _, th := range pr.ReviewThreads.Nodes {
		if th.IsResolved {
			continue
		}

		req := Comment{Path: th.Path, Line: cmp.Or(th.Line, th.OriginalLine)}

		var bodies []string

		for _, cm := range th.Comments.Nodes {
			ok, err := allowed(cm.Author.Login)
			if err != nil {
				return nil, err
			}

			if ok {
				req.ID, req.Author = cm.ID, cm.Author.Login
				bodies = append(bodies, cut(cm.Body, bodyCap))
			}
		}

		if len(bodies) > 0 {
			req.Body = strings.Join(bodies, "\n\n")
			out = append(out, req)
		}
	}

	return out, nil
}

// cut is s cut to at most n bytes on a character's boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
