package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	maxLine = 1 << 20
	maxJSON = 4 << 20
)

type wireUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

type wireEvent struct {
	Type    string     `json:"type"`
	Model   string     `json:"model"`
	Usage   *wireUsage `json:"usage"`
	Message struct {
		Model string     `json:"model"`
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
}

// meter reads a copy of a response body for token usage. It never touches the
// bytes the client receives.
type meter struct {
	sse      bool
	line     []byte
	skipLine bool
	buf      []byte
	overflow bool
	usage    Usage
	seen     bool
}

func (m *meter) write(p []byte) {
	if !m.sse {
		if m.overflow {
			return
		}

		if len(m.buf)+len(p) > maxJSON {
			m.overflow, m.buf = true, nil

			return
		}

		m.buf = append(m.buf, p...)

		return
	}

	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.appendLine(p)

			return
		}

		m.appendLine(p[:i])
		m.endLine()

		p = p[i+1:]
	}
}

func (m *meter) appendLine(chunk []byte) {
	if m.skipLine {
		return
	}

	if len(m.line)+len(chunk) > maxLine {
		m.skipLine, m.line = true, nil

		return
	}

	m.line = append(m.line, chunk...)
}

func (m *meter) endLine() {
	if !m.skipLine {
		m.handleLine(m.line)
	}

	m.line, m.skipLine = m.line[:0], false
}

func (m *meter) handleLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
	if !ok {
		return
	}

	var ev wireEvent
	if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
		return
	}

	switch ev.Type {
	case "message_start":
		if u := ev.Message.Usage; u != nil {
			m.usage = Usage{
				Model: ev.Message.Model, Input: u.Input, Output: u.Output,
				CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			}
			m.seen = true
		}
	case "message_delta":
		if u := ev.Usage; u != nil {
			m.usage.Input = nonZero(u.Input, m.usage.Input)
			m.usage.Output = nonZero(u.Output, m.usage.Output)
			m.usage.CacheRead = nonZero(u.CacheRead, m.usage.CacheRead)
			m.usage.CacheWrite = nonZero(u.CacheWrite, m.usage.CacheWrite)
			m.seen = true
		}
	}
}

func nonZero(v, old int64) int64 {
	if v != 0 {
		return v
	}

	return old
}

// result finishes the stream and returns what was seen.
func (m *meter) result() (Usage, bool) {
	if m.sse {
		if !m.skipLine && len(m.line) > 0 {
			m.handleLine(m.line)
		}

		return m.usage, m.seen
	}

	if m.overflow {
		return Usage{}, false
	}

	var ev wireEvent
	if json.Unmarshal(m.buf, &ev) != nil || ev.Usage == nil {
		return Usage{}, false
	}

	return Usage{
		Model: ev.Model, Input: ev.Usage.Input, Output: ev.Usage.Output,
		CacheRead: ev.Usage.CacheRead, CacheWrite: ev.Usage.CacheWrite,
	}, true
}

// meterBody passes bytes through unchanged and reports usage on Close.
type meterBody struct {
	rc io.ReadCloser
	m  *meter // nil when the response is not metered
	h  *handler
}

func encoded(res *http.Response) bool {
	enc := res.Header.Get("Content-Encoding")

	return enc != "" && !strings.EqualFold(enc, "identity")
}

func newMeterBody(res *http.Response, h *handler) io.ReadCloser {
	b := &meterBody{rc: res.Body, h: h}

	if res.StatusCode >= 400 || encoded(res) {
		return b
	}

	switch mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt {
	case "text/event-stream":
		b.m = &meter{sse: true}
	case "application/json":
		b.m = &meter{}
	}

	return b
}

func (b *meterBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.m != nil {
		b.m.write(p[:n])
	}

	return n, err
}

func (b *meterBody) Close() error {
	err := b.rc.Close()

	if b.m != nil {
		if u, ok := b.m.result(); ok {
			b.m = nil
			b.h.usage(u)
		}
	}

	return err
}
