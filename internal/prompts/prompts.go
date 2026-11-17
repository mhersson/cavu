// Package prompts holds CAVU's own prompts to the agent and the schemas of the
// results a session ends with, as embedded files.
package prompts

import (
	"embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

// Prompt names, each a file of this package. Instructions, appended to the
// system prompt of every session, Continue, Doctor and Proceed take no data.
const (
	Instructions = "instructions"
	Work         = "work"      // a session's opening prompt
	Readiness    = "readiness" // the opening prompt of a session that starts with the readiness turn
	Proceed      = "proceed"   // sent when the readiness turn found nothing missing
	Continue     = "continue"
	FixVerify    = "fix_verify"
	Answer       = "answer"
	Doctor       = "doctor" // the health check's session
)

// WorkData fills the work and readiness prompts.
type WorkData struct{ Title, Body string }

// FixVerifyData fills the fix_verify prompt. Output is the recorded verify
// output, capped at 64 KiB with its head and tail kept.
type FixVerifyData struct{ Command, Output string }

// AnswerData fills the answer prompt.
type AnswerData struct{ Answer string }

var (
	//go:embed *.md
	files embed.FS

	//go:embed result.schema.json
	resultSchema []byte

	//go:embed readiness.schema.json
	readinessSchema []byte

	templates = template.Must(template.New("").Option("missingkey=error").ParseFS(files, "*.md"))
)

// Render fills the named prompt with data. A placeholder data does not fill
// is an error.
func Render(name string, data any) (string, error) {
	t := templates.Lookup(name + ".md")
	if t == nil {
		return "", fmt.Errorf("prompts: no prompt %q", name)
	}

	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("prompts: %s: %w", name, err)
	}

	return b.String(), nil
}

// ResultSchema returns the JSON Schema of a session's structured result, a
// copy the caller may change.
func ResultSchema() json.RawMessage { return slices.Clone(resultSchema) }

// ReadinessSchema returns the JSON Schema of a readiness turn's structured
// result, a copy the caller may change.
func ReadinessSchema() json.RawMessage { return slices.Clone(readinessSchema) }
