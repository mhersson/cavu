package engine

import (
	"context"
	"time"

	"github.com/mhersson/cavu/internal/store"
)

// RunConfig is everything a runner needs for one run. Serialisable: a
// networked runner will receive it over the wire.
type RunConfig struct {
	TaskID, RunID, ProjectID int64
	InstanceID, Image        string
	Repo                     struct{ URL, BaseBranch, BaseCommit, Branch, Token string }
	Setup                    struct {
		Command       string
		Env, Secrets  map[string]string
		ToolOverrides map[string]string
		EgressHosts   []string
	}
	Verify struct {
		Command        string
		Waived         bool
		Timeout        time.Duration
		BaselineCommit string
		BaselinePassed bool
	}
	Endpoint struct{ Mode, URL, AuthHeader, Credential string }
	Agent    struct {
		Model, Effort  string
		Review, Strong struct{ Model, Effort string } // each falls back on the work model and effort
		MaxTurns       int
		Timeout        time.Duration
		WebTools       bool
	}
	Identity struct{ Name, Email string }
	Task     struct{ Title, Body string }
	// Ecosystems are what the run's prepare detected, for Work, Verify and
	// Export; never a setting, and empty for Prepare.
	Ecosystems []string
}

// RunRef names a run's containers and volumes. It holds no secret and no
// setting but the instance id, so a release never waits on one that can no
// longer be read. Image is the loop's own, never a setting: a runner needs it
// even to remove a run's containers.
type RunRef struct {
	InstanceID, Image        string
	TaskID, RunID, ProjectID int64
}

// Runner carries out a run's actions where a container runtime exists.
type Runner interface {
	// Prepare starts from empty run volumes: clone, toolchains, setup, baseline
	// verify. Work, Review, Verify and Export bring the run's containers up
	// themselves when they are not running, so a restart needs no second prepare.
	Prepare(ctx context.Context, c RunConfig) (PrepareResult, error)
	// Work returns how the session ended as a result, endpoint failures and
	// usage limits included; an error means the sandbox itself failed, and
	// comes with the session's Usage and Turns so far. A resumed session that
	// does not exist is started under its id with the task's opening prompt,
	// whatever the mode: the process died, ctx ended or the sandbox failed
	// between the claim and the start. progress is never called after Work returns.
	Work(ctx context.Context, c RunConfig, w WorkRequest, progress func(Progress)) (WorkResult, error)
	Verify(ctx context.Context, c RunConfig) (VerifyResult, error)
	// Review runs one read-only review session on the frozen head and changes
	// nothing. An error comes with the session's Usage and Turns so far.
	Review(ctx context.Context, c RunConfig, r ReviewRequest, progress func(Progress)) (ReviewResult, error)
	// Export exports commit, the work a session froze, never the working tree.
	// It is safe to repeat for the same run.
	Export(ctx context.Context, c RunConfig, commit string) (ExportResult, error)
	// Release is safe to repeat, and succeeds when nothing exists to release.
	Release(ctx context.Context, ref RunRef, removeVolumes bool) error
	// CheckEnvironment prepares a project as a run does, under run and task id
	// 0 and never reusing a baseline, then releases it with its volumes. It
	// also returns the verify command the repository suggests, or "".
	CheckEnvironment(ctx context.Context, c RunConfig) (res PrepareResult, verifySuggestion string, err error)
}

// PrepareResult is what preparing a run found. A failed step is a result, not an error.
type PrepareResult struct {
	BaseCommit   string
	Tools        []string
	IgnoredTools []string // repository tool entries detection declined to install, as written
	Ecosystems   []string // detected at the base commit; later actions of the run get them back
	Baseline     string   // passed, reused, waived, failed; a failed baseline sets FailedStep to "baseline"
	FailedStep   string   // "", clone, toolchain, setup, baseline
	Output       string
}

// WorkRequest starts or resumes an agent session.
type WorkRequest struct {
	Mode, SessionID string
	Resume          bool
	Role            string    // work or strong, which picks the model and effort
	Detail          string    // the verify output for fix_verify, the answer for answer
	Findings        []Finding // fix_review: the open findings
}

// ReviewRequest starts a review session, which never resumes.
type ReviewRequest struct {
	SessionID            string    // new
	Head                 string    // the frozen commit under review
	FixBase              string    // scoped: the commit the previous review saw
	Findings             []Finding // scoped: the open findings to rule on
	Answers, Assumptions []string
	Summary              string // the working session's own summary: claims, not facts
}

// Progress is something a session reported before it ended.
type Progress struct {
	Type    string // an event type; the loop records agent_text, agent_tool, rate_limit and egress_denied
	Payload any
}

// WorkResult is how a session ended.
type WorkResult struct {
	Outcome                string
	Title, Summary         string
	Detail                 string // the upstream status or error text for endpoint, usage_limit and crashed
	Assumptions, Questions []string
	SuggestedAnswers       []string // one per question, the answer the agent would assume
	Changed                bool
	HeadCommit             string // the commit the session's work was frozen into
	Turns                  int
	CostEstimateUSD        float64
	CostMissing            bool // the session ended without a cost estimate
	ResetsAt               time.Time
	Usage                  []store.Usage
}

// ReviewResult is how a review ended: reviewed, failed, or a session outcome that stops.
type ReviewResult struct {
	Outcome         string
	Asked, Built    Verdict
	Findings        []Finding
	Addressed       []bool
	Detail          string
	Turns           int
	CostEstimateUSD float64
	CostMissing     bool // the session ended without a cost estimate
	ResetsAt        time.Time
	Usage           []store.Usage
}

// VerifyResult is the outcome of the verify command.
type VerifyResult struct {
	Result, Command, Output string
	ExitCode                int
	Duration                time.Duration
}

// ExportResult is the exported change. The artifacts come back as data,
// because a runner on another machine cannot write the server's files.
type ExportResult struct {
	HeadCommit   string
	Diff, Bundle []byte
	FilesChanged int
	DiffStat     []string // git's summary of the diff: a line per file, a long list cut short, then the totals
	Problem      string   // non-empty: stop as agent_error with this text
}
