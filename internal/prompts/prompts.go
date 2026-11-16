// Package prompts holds CAVU's own prompts to the agent and the schema of the
// result a session ends with, as embedded files.
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
// system prompt of every session, and Continue take no data.
const (
	Instructions = "instructions"
	Work         = "work" // a session's opening prompt
	Continue     = "continue"
	FixVerify    = "fix_verify"
	Answer       = "answer"
)

// WorkData fills the work prompt.
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
