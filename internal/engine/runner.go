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
		Model, Effort string
		MaxTurns      int
		Timeout       time.Duration
		WebTools      bool
	}
	Identity    struct{ Name, Email string }
	CABundle    []byte
	Task        struct{ Title, Body string }
	ArtifactDir string
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
	// verify. Work, Verify and Export bring the run's containers up themselves
	// when they are not running, so a restart needs no second prepare.
	Prepare(ctx context.Context, c RunConfig) (PrepareResult, error)
	// Work returns how the session ended as a result, endpoint failures and
	// usage limits included; an error means the sandbox itself failed. A
	// resumed session that does not exist is started under its id with the
	// task's opening prompt, whatever the mode: the process died, or ctx
	// ended, between the claim and the start. progress is never called after
	// Work returns.
	Work(ctx context.Context, c RunConfig, w WorkRequest, progress func(Progress)) (WorkResult, error)
	Verify(ctx context.Context, c RunConfig) (VerifyResult, error)
	// Export is safe to repeat for the same run.
	Export(ctx context.Context, c RunConfig, title string) (ExportResult, error)
	// Release is safe to repeat, and succeeds when nothing exists to release.
	Release(ctx context.Context, ref RunRef, removeVolumes bool) error
}

// PrepareResult is what preparing a run found. A failed step is a result, not an error.
type PrepareResult struct {
	BaseCommit   string
	Tools        []string
	IgnoredTools []string // repository tool entries detection declined to install, as written
	Baseline     string   // passed, reused, waived, failed; a failed baseline sets FailedStep to "baseline"
	FailedStep   string   // "", clone, toolchain, setup, baseline
	Output       string
}

// WorkRequest starts or resumes an agent session.
type WorkRequest struct {
	Mode, SessionID string
	Resume          bool
	Detail          string // the verify output for fix_verify, the answer for answer
}

// Progress is something a session reported before it ended.
type Progress struct {
	Type    string // an event type; the loop records agent_text, agent_tool, rate_limit and egress_denied
	Payload any
}

// WorkResult is how a session ended.
type WorkResult struct {
	SessionID              string // the id the agent reported; the engine's own id stays authoritative
	Outcome                string
	Title, Summary         string
	Detail                 string // the upstream status or error text for endpoint, usage_limit and crashed
	Assumptions, Questions []string
	Changed                bool
	Turns                  int
	CostEstimateUSD        float64
	ResetsAt               time.Time
	Usage                  []store.Usage
}

// VerifyResult is the outcome of the verify command.
type VerifyResult struct {
	Result, Command, Output string
	ExitCode                int
	Duration                time.Duration
}

// ExportResult is the exported change.
type ExportResult struct {
	HeadCommit, DiffPath, BundlePath string
	FilesChanged                     int
	Problem                          string // non-empty: stop as agent_error with this text
}
