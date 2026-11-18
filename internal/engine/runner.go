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
	// Repo is the project's repository and the task's branch. Head is the
	// branch's commit a run starts from, "" for a run from the base commit.
	Repo struct {
		URL, BaseBranch, BaseCommit, Branch, Head string
		Access                                    ForgeAccess
		IgnoredChecks, ReviewBots                 []string // the project's forge policy
	}
	Setup struct {
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
		// Instance is what the instance gives every session: its instruction
		// file's text, and its skills directory's files by their path under it.
		Instance struct {
			Instructions string
			Skills       map[string]File
		}
	}
	Identity struct{ Name, Email string }
	Task     struct{ Title, Body string }
	// Budget is what the run's model calls may still spend, set in key mode
	// while a money cap is in force; the proxy counts it against Prices.
	Budget struct {
		Capped   bool
		MicroUSD int64
		Prices   map[string]Price
	}
	// Ecosystems are what the run's prepare detected, for Work, Verify and
	// Export; never a setting, and empty for Prepare.
	Ecosystems []string
}

// ForgeAccess is the credential for the forge a project's repository is on:
// a personal access token, or a GitHub App's id and private key (PEM). Its
// fields are forge.Access's, in the same order, so the runner converts it.
type ForgeAccess struct {
	Token  string
	AppID  int64
	AppKey string
}

// Comment is a reviewer's request on the task's pull request. ID is a
// review's id, or the id of a thread's newest comment by an allowed author.
// Its fields are forge.Comment's, in the same order, so the runner converts it.
type Comment struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Body   string `json:"body"`
}

// File is one file of the instance's skills directory.
type File struct {
	Data       []byte
	Executable bool
}

// RunRef names a run's containers and volumes. It holds no secret and no
// setting but the instance id, so a release never waits on one that can no
// longer be read. Image is the loop's own, never a setting: a runner needs it
// even to remove a run's containers.
type RunRef struct {
	InstanceID, Image        string
	TaskID, RunID, ProjectID int64
}

// Remove is what a release removes besides the run's containers.
type Remove int

const (
	KeepVolumes Remove = iota // stop the containers
	RemoveRun                 // and remove the run's volumes
	RemoveAll                 // and the task's home
)

// Runner carries out a run's actions where a container runtime exists.
type Runner interface {
	// Prepare starts from empty run volumes: clone, toolchains, setup, baseline
	// verify. Work, Review, Verify and Export bring the run's containers up
	// themselves when they are not running, so a restart needs no second prepare.
	// A prepare at a head keeps the task's home, runs no baseline and ends with
	// the workspace at the head.
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
	// nothing in the workspace. An error comes with the session's Usage and
	// Turns so far.
	Review(ctx context.Context, c RunConfig, r ReviewRequest, progress func(Progress)) (ReviewResult, error)
	// Export exports commit, the work a session froze, never the working tree.
	// It is safe to repeat for the same run.
	Export(ctx context.Context, c RunConfig, commit string) (ExportResult, error)
	// Publish imports an exported change's bundle into the project's mirror,
	// pushes its head to the task's branch and, for a first publish, opens a
	// draft pull request. A rejection is a result. Each part is safe to repeat,
	// so a retry finishes a publish that failed half way.
	Publish(ctx context.Context, c RunConfig, p PublishRequest) (PublishResult, error)
	// Release is safe to repeat, and succeeds when nothing exists to release.
	Release(ctx context.Context, ref RunRef, remove Remove) error
	// Observe reads the task's pull request number from the forge. Observe,
	// Ready and Reply start no container, and return a *RateLimited while the
	// forge limits their requests.
	Observe(ctx context.Context, c RunConfig, number int) (Observation, error)
	// Ready marks the pull request with node id nodeID ready for review; one
	// that is ready already is no change.
	Ready(ctx context.Context, c RunConfig, nodeID string) error
	// Reply posts body as one comment on pull request number, never in a
	// review thread, with the run's secrets masked.
	Reply(ctx context.Context, c RunConfig, number int, body string) error
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
	Baseline     string   // passed, reused, waived, failed, or skipped by a prepare at a head; a failed baseline sets FailedStep to "baseline"
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
	Comments        []Comment // comments: the reviewers' open requests
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
	WhereToLook            []string // the parts of the change the session says need a person's eyes first
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
	DiffStat     []string      // git's summary of the diff: a line per file, a long list cut short, then the totals
	Files        []ChangedFile // every path the change adds, modifies or deletes, a rename as a delete and an add
	Problem      string        // non-empty: stop as agent_error with this text
}

// PublishRequest is an exported change to publish.
type PublishRequest struct {
	Bundle             []byte
	Head, Prev, Branch string // Prev: the head published before, "" for a first publish
	Number             int    // the task's pull request, 0 before the first publish
	Title, Body        string
}

// PublishResult is what a publish did: the branch the push took, the head on
// it, and the pull request.
type PublishResult struct {
	Branch, Head string
	Number       int
	URL, NodeID  string
	Rejected     string // non-empty: stop as push_rejected with this text
}

// ChangedFile is a path a change touches.
type ChangedFile struct {
	Path  string
	Added bool
	Size  int64 // of the file as the change leaves it, 0 for a deleted one
}
