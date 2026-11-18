package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	// logTail is how much of the end of a job's log FailedLogs keeps.
	logTail = 32 << 10
	// logsCap is the cap the verify output in a fix prompt already has.
	logsCap = 64 << 10
)

// FailedLogs is what a fix session reads of head's failed checks, at most 64 KiB: for an Actions
// job its name and the end of its log, for any other check its name, output and address. Whole
// entries are added in name order until the cap, and a last line names those that did not fit.
func (c *Client) FailedLogs(ctx context.Context, r Repo, a Access, head string, ignored []string) (string, error) {
	token, err := c.Token(ctx, r, a, false)
	if err != nil {
		return "", err
	}

	checks, err := c.checks(ctx, r, token, head, ignored)
	if err != nil {
		return "", err
	}

	failed := slices.DeleteFunc(checks, func(ch check) bool { return ch.state != checkFailed })
	slices.SortFunc(failed, func(a, b check) int { return strings.Compare(a.name, b.name) })

	var b strings.Builder

	for i, ch := range failed {
		e, err := c.entry(ctx, r, token, ch)
		if err != nil {
			return "", err
		}

		if b.Len()+len(e)+len(leftOut(failed[i+1:])) > logsCap {
			b.WriteString(leftOut(failed[i:]))

			break
		}

		b.WriteString(e)
	}

	return b.String(), nil
}

// entry is a failed check's part of FailedLogs.
func (c *Client) entry(ctx context.Context, r Repo, token string, ch check) (string, error) {
	parts := []string{"## " + ch.name}

	if ch.app == "github-actions" {
		log, err := c.jobLog(ctx, r, token, ch.id)
		if err != nil {
			return "", err
		}

		parts = append(parts, log)
	} else {
		for _, s := range []string{ch.title, ch.summary, ch.url} {
			if s != "" {
				parts = append(parts, s)
			}
		}
	}

	return strings.Join(parts, "\n\n") + "\n\n", nil
}

// leftOut is the line naming the checks that did not fit, "" for none.
func leftOut(rest []check) string {
	if len(rest) == 0 {
		return ""
	}

	names := make([]string, len(rest))
	for i, ch := range rest {
		names[i] = ch.name
	}

	return "Left out, past 64 KiB: " + strings.Join(names, ", ") + "\n"
}

// jobLog is the end of Actions job id's log, "" for a log the forge holds no
// bytes of. The forge redirects to a storage host, and errors never quote its
// signed address.
func (c *Client) jobLog(ctx context.Context, r Repo, token string, id int64) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("%s%s/actions/jobs/%d/logs", r.restURL(), r.path(), id), token, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Range", "bytes=-"+strconv.Itoa(logTail))

	resp, err := c.do(req)
	if he, ok := errors.AsType[*httpError](err); ok && he.status == http.StatusRequestedRangeNotSatisfiable {
		return "", nil
	}

	if err != nil {
		return "", err
	}

	// A server that ignores Range sends the whole log.
	return string(resp.body[max(0, len(resp.body)-logTail):]), nil
}
