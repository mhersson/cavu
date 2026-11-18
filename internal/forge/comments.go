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
      latestOpinionatedReviews(last: 100) { nodes { id state body author { __typename login } } }
      reviewThreads(last: 100) {
        nodes {
          isResolved path line originalLine
          comments(last: 100) { nodes { id state body author { __typename login } } }
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
	Typename string `json:"__typename"`
	Login    string `json:"login"`
}

// name is the author as GitHub's REST API and page write it, a bot as
// name[bot]; GraphQL's documentation does not say which form a Bot's login has.
func (a author) name() string {
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		return a.Login + "[bot]"
	}

	return a.Login
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
					State  string `json:"state"` // PENDING while its review is not submitted
					Body   string `json:"body"`
					Author author `json:"author"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

// comments reads the reviewer requests on pull request q.Number. An author is
// allowed when q.ReviewBots names it, a bot as name[bot] and a user by login,
// else when the forge says the user may write; each user is asked once.
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
		known[strings.ToLower(b)] = true
	}

	allowed := func(a author) (bool, error) {
		k := strings.ToLower(a.name())

		switch ok, seen := known[k]; {
		case a.Login == "": // a deleted account
			return false, nil
		case seen:
			return ok, nil
		case a.Typename == "Bot": // no collaborator, so allowed by name alone
			return false, nil
		}

		ok, err := c.writer(ctx, r, token, a.Login)
		known[k] = ok

		return ok, err
	}

	return requests(d.Repository.PullRequest, allowed)
}

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
// comments' bodies. A comment of a review not yet submitted is left out, and
// neither is held to a commit, so an outdated thread counts.
func requests(pr pullRequestNode, allowed func(author) (bool, error)) ([]Comment, error) {
	var out []Comment

	for _, rv := range pr.LatestOpinionatedReviews.Nodes {
		if rv.State != "CHANGES_REQUESTED" || rv.Body == "" {
			continue
		}

		ok, err := allowed(rv.Author)
		if err != nil {
			return nil, err
		}

		if ok {
			out = append(out, Comment{ID: rv.ID, Author: rv.Author.name(), Body: cut(rv.Body, bodyCap)})
		}
	}

	for _, th := range pr.ReviewThreads.Nodes {
		if th.IsResolved {
			continue
		}

		req := Comment{Path: th.Path, Line: cmp.Or(th.Line, th.OriginalLine)}

		var bodies []string

		for _, cm := range th.Comments.Nodes {
			if cm.State == "PENDING" {
				continue
			}

			ok, err := allowed(cm.Author)
			if err != nil {
				return nil, err
			}

			if ok {
				req.ID, req.Author = cm.ID, cm.Author.name()
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
