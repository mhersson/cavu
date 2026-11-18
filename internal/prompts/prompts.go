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
// system prompt of every session that may write, Continue, Doctor and Proceed
// take no data. A read-only session's prompt says all it needs.
const (
	Instructions   = "instructions"
	Work           = "work"      // a session's opening prompt
	Readiness      = "readiness" // the opening prompt of a session that starts with the readiness turn
	Proceed        = "proceed"   // sent when the readiness turn found nothing missing
	Continue       = "continue"
	FixVerify      = "fix_verify"
	Answer         = "answer"
	Doctor         = "doctor"           // the health check's session
	Review         = "review"           // a full review; takes ReviewData
	Rereview       = "rereview"         // a scoped review of one fix; takes RereviewData
	FixReview      = "fix_review"       // the findings, to the session that did the work; takes FixReviewData
	FixReviewFresh = "fix_review_fresh" // the task, the diff and the findings, to a new session; takes FixReviewFreshData
	FixCI          = "fix_ci"           // what the pull request's failed checks printed, to the session that did the work; takes FixCIData
	Comments       = "comments"         // a reviewer's requests on the pull request, to the session that did the work; takes CommentsData
)

// WorkData fills the work and readiness prompts.
type WorkData struct{ Title, Body string }

// ReviewFinding is a finding as the prompts give it: a rereview's verdicts
// name it by Number.
type ReviewFinding struct {
	Number                     int
	Severity, Where, What, Why string
}

// ReviewData fills the review prompt. Summary is the working session's own
// account of its change; DiffFile holds the change. VerifyCommand is the
// command the change passed, unless VerifyWaived says the project has none.
type ReviewData struct {
	Title, Body          string
	Answers, Assumptions []string
	Summary, DiffFile    string
	VerifyCommand        string
	VerifyWaived         bool
}

// RereviewData fills the rereview prompt: the findings left open, the whole
// change in DiffFile and the fix in FixDiffFile. VerifyCommand is the command
// the fix passed, unless VerifyWaived says the project has none.
type RereviewData struct {
	Title, Body           string
	Findings              []ReviewFinding
	DiffFile, FixDiffFile string
	VerifyCommand         string
	VerifyWaived          bool
}

// FixReviewData fills the fix_review prompt.
type FixReviewData struct{ Findings []ReviewFinding }

// FixReviewFreshData fills the fix_review_fresh prompt; DiffFile holds the
// change so far.
type FixReviewFreshData struct {
	Title, Body string
	Findings    []ReviewFinding
	DiffFile    string
}

// FixVerifyData fills the fix_verify prompt. Output is the recorded verify
// output, capped at 64 KiB with its head and tail kept.
type FixVerifyData struct{ Command, Output string }

// AnswerData fills the answer prompt.
type AnswerData struct{ Answer string }

// FixCIData fills the fix_ci prompt. Logs is what the failed checks printed.
type FixCIData struct{ Logs string }

// ReviewComment is a reviewer's request as the comments prompt gives it: Path
// is "" for a request about the whole change, and Line 0 for one on no line.
type ReviewComment struct {
	Author, Path string
	Line         int
	Body         string
}

// CommentsData fills the comments prompt.
type CommentsData struct{ Comments []ReviewComment }

var (
	//go:embed *.md
	files embed.FS

	//go:embed result.schema.json
	resultSchema []byte

	//go:embed readiness.schema.json
	readinessSchema []byte

	//go:embed review.schema.json
	reviewSchema []byte

	//go:embed rereview.schema.json
	rereviewSchema []byte

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

// ReviewSchema returns the JSON Schema of a full review's structured result,
// a copy the caller may change.
func ReviewSchema() json.RawMessage { return slices.Clone(reviewSchema) }

// RereviewSchema returns the JSON Schema of a scoped review's structured
// result, a copy the caller may change.
func RereviewSchema() json.RawMessage { return slices.Clone(rereviewSchema) }
