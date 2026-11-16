package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mhersson/cavu/internal/driver"
	"github.com/mhersson/cavu/internal/proc"
)

// noConversation is what the pinned release prints on stderr when --resume
// names a session whose transcript is missing.
const noConversation = "No conversation found"

// Run starts or resumes one session as one claude process. The error is only
// for a spec the driver cannot run or a target that cannot start it; every
// other end is a Result.
func (c claudeCode) Run(ctx context.Context, t proc.Target, s driver.Spec, sink func(driver.Event)) (driver.Result, error) {
	inv, err := c.newInvocation(s)
	if err != nil {
		return driver.Result{}, err
	}

	m := newMapper([]string{s.Auth.Token}, sink)

	var p proc.Process
	if err = ctx.Err(); err == nil {
		p, err = t.Start(ctx, proc.Command{Argv: inv.argv, Env: inv.env, Dir: s.Dir, Files: inv.files})
	}

	if err != nil {
		if ctx.Err() != nil {
			return driver.Result{Reason: driver.ReasonInterrupted, IsError: true, Detail: m.mask(interruption(ctx))}, nil
		}

		return driver.Result{}, fmt.Errorf("claudecode: start: %w", err)
	}

	r := &session{p: p, m: m, secret: s.Auth.Token, mode: s.Auth.Mode, resume: s.Resume != "", grace: c.grace}

	return r.supervise(ctx, c.gateTimeout, inv.prompt), nil
}

func interruption(ctx context.Context) string {
	return "interrupted: " + context.Cause(ctx).Error()
}

// session is one started claude process. Only Run's goroutine feeds the
// mapper, so the sink never sees an event after Run returns.
type session struct {
	p      proc.Process
	m      *mapper
	secret string
	mode   string
	resume bool
	grace  time.Duration
	// wrote is closed when the prompt write returns; nil until it starts.
	wrote   chan struct{}
	waitErr error
}

// supervise passes the initialize gate and the account check, writes the
// prompt, and feeds stdout to the mapper until the run ends.
func (r *session) supervise(ctx context.Context, gate time.Duration, prompt []byte) driver.Result {
	id := "init-" + randomID()

	if _, err := r.p.Stdin().Write(initRequest(id)); err != nil {
		// The process is already gone; its exit status and stderr say why.
		return r.ended(ctx)
	}

	timer := time.NewTimer(gate)
	defer timer.Stop()

	gateC := timer.C

	for {
		select {
		case line, ok := <-r.p.Lines():
			if !ok {
				return r.ended(ctx)
			}

			if gateC != nil {
				if acct, isInit := initResponse(line, id); isInit {
					if why := accountMismatch(r.mode, acct); why != "" {
						return r.abort(driver.ReasonAuth, why)
					}

					gateC = nil

					r.writePrompt(prompt)

					continue
				}
			}

			if r.m.feed(line) {
				return r.ended(ctx)
			}
		case <-gateC:
			return r.abort(driver.ReasonCrashed, fmt.Sprintf("no answer to the initialize request within %s", gate))
		case <-ctx.Done():
			return r.abort(driver.ReasonInterrupted, interruption(ctx))
		}
	}
}

// writePrompt writes while stdout keeps draining, since a large prompt can
// fill the pipe. A failed write means the process is gone; its exit status
// says why.
func (r *session) writePrompt(prompt []byte) {
	r.wrote = make(chan struct{})

	go func() {
		defer close(r.wrote)

		_, _ = r.p.Stdin().Write(prompt)
	}()
}

// ended closes a run whose stream ended or whose mapper asked to stop. A cancel
// that arrives after the result line keeps that result's outcome.
func (r *session) ended(ctx context.Context) driver.Result {
	code := r.end(ctx, r.m.sawResult)
	tail := r.p.StderrTail()

	detail := stderrDetail(tail, r.secret)
	if r.waitErr != nil {
		detail = strings.TrimSpace(detail + "\n" + r.waitErr.Error())
	}

	res := r.m.finish(code, detail)

	if !r.m.sawResult && r.resume && strings.Contains(tail, noConversation) {
		res.Reason = driver.ReasonResumeRefused
	}

	return res
}

// abort stops the process and ends the run with reason, whatever the stream
// showed so far.
func (r *session) abort(reason, why string) driver.Result {
	code := r.end(context.Background(), false)

	tail := stderrDetail(r.p.StderrTail(), r.secret)
	res := r.m.finish(code, tail)

	if tail != "" {
		why += "; stderr: " + tail
	}

	res.Reason, res.IsError, res.Detail = reason, true, r.m.mask(why)

	return res
}

// end stops the process and returns its exit code. After a result the
// process gets the grace to exit on its own once its stdin closes.
func (r *session) end(ctx context.Context, graceful bool) int {
	if graceful {
		_ = r.p.Stdin().Close()

		r.drain(ctx)
	}

	_ = r.p.Stop(r.grace)
	// Unblocks a prompt write the process never read.
	_ = r.p.Stdin().Close()

	if r.wrote != nil {
		<-r.wrote
	}

	code, err := r.p.Wait()
	r.waitErr = err

	return code
}

// drain discards stdout until it closes, the grace runs out or ctx ends.
func (r *session) drain(ctx context.Context) {
	timer := time.NewTimer(r.grace)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-r.p.Lines():
			if !ok {
				return
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

func initRequest(id string) []byte {
	b, _ := json.Marshal(map[string]any{ //nolint:errchkjson // strings always marshal
		"type": "control_request", "request_id": id, "request": map[string]string{"subtype": "initialize"},
	})

	return append(b, '\n')
}

// initResponse reports whether line answers initialize request id, and the
// account it reports, undecoded so a changed shape still ends the gate.
func initResponse(line []byte, id string) (json.RawMessage, bool) {
	if !bytes.Contains(line, []byte(`"control_response"`)) {
		return nil, false
	}

	var l struct {
		Type     string `json:"type"`
		Response struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Account json.RawMessage `json:"account"`
			} `json:"response"`
		} `json:"response"`
	}

	if json.Unmarshal(line, &l) != nil || l.Type != "control_response" || l.Response.RequestID != id {
		return nil, false
	}

	return l.Response.Response.Account, true
}

// stderrDetail drops a leading proper suffix of secret, which a tail cut at a
// byte count can open with and the mask cannot see. Whether the tail was cut
// is unknown, so an uncut one that opens that way loses those bytes too.
func stderrDetail(tail, secret string) string {
	for n := len(secret) - 1; n > 0; n-- {
		if strings.HasPrefix(tail, secret[len(secret)-n:]) {
			tail = tail[n:]

			break
		}
	}

	return strings.TrimSpace(strings.ToValidUTF8(tail, ""))
}
