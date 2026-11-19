package engine

// Status is where a task stands, as a person sees it.
type Status string

// The status scale.
const (
	StatusDraft     Status = "draft"
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusBlocked   Status = "blocked"
	StatusInReview  Status = "in_review"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
)

// StatusOf derives a task's status from its facts.
func StatusOf(f Facts) Status {
	switch {
	case f.Cancelled:
		return StatusCancelled
	case !f.Started:
		return StatusDraft
	case f.Merged:
		return StatusDone
	case f.OpenStop != nil:
		return StatusBlocked
	case f.Exported, f.PR != nil && !f.RunOpen:
		return StatusInReview
	case !f.RunOpen:
		return StatusQueued
	default:
		return StatusRunning
	}
}

// NeedsYou reports whether a task waits for a person: a stop that only a person
// clears or a fault past its retries, a ready pull request, or a change no
// publish carried out. It reads facts and caps and chooses nothing.
func NeedsYou(s Snapshot) bool {
	f := s.Facts

	switch StatusOf(f) {
	case StatusBlocked:
		return f.OpenStop.Action != ClearNone && !f.retriesFault(s.Caps)
	case StatusInReview:
		if f.PR == nil {
			return s.Task.ReplayID == 0
		}

		return PullRequestOf(f, s.Watch.NoCI).Ready
	default:
		return false
	}
}

// PullRequest is a task's pull request as a person sees it.
type PullRequest struct {
	Number int      `json:"number"`
	URL    string   `json:"url"`
	Branch string   `json:"branch"`
	Head   string   `json:"head"`   // the pull request's head as CAVU has it
	State  string   `json:"state"`  // PROpen, PRMerged or PRClosed as the newest look has it; PROpen before any look
	Ready  bool     `json:"ready"`  // open, no draft, checks passed (or none, with no CI), and the task waits for a person, not for a follow-up run
	Checks string   `json:"checks"` // a Checks* value of the head; "" until a look showed it
	Failed []string `json:"failed_checks"`
}

// PullRequestOf reads the task's pull request, nil before its first publish.
// The checks are those of its own head: until a look shows that head, as
// between a publish and the first look at it or when the newest look shows
// another commit, they are not read yet. Ready also needs a task that waits
// for a person: no run or stop open, neither merged nor cancelled, and no
// request of a reviewer waiting for a follow-up run.
func PullRequestOf(f Facts, noCI bool) *PullRequest {
	pr := f.PR
	if pr == nil {
		return nil
	}

	v := &PullRequest{Number: pr.Number, URL: pr.URL, Branch: pr.Branch, Head: pr.Head, State: PROpen}

	seen := pr.Seen
	if seen == nil {
		return v
	}

	v.State = seen.State

	if seen.Head == pr.Head {
		v.Checks, v.Failed = seen.Checks, seen.Failed
		v.Ready = !f.RunOpen && f.OpenStop == nil && !f.Merged && !f.Cancelled &&
			seen.State == PROpen && !seen.Draft && len(unanswered(pr)) == 0 && (seen.Checks == ChecksPassed || seen.Checks == ChecksNone && noCI)
	}

	return v
}

// ChangeOf is the change a task shows: the export CAVU published last whenever
// the task has a pull request, a fix run in the middle of its work included,
// else the newest export, else nil.
func ChangeOf(f Facts) *ExportedPayload {
	switch {
	case f.PR != nil:
		return &f.PR.Export
	case f.Exported:
		return &f.Export
	default:
		return nil
	}
}
