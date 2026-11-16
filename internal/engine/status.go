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

// StatusOf derives a task's status from its facts. It never returns
// StatusQueued or StatusDone.
func StatusOf(f Facts) Status {
	switch {
	case f.Cancelled:
		return StatusCancelled
	case !f.Started:
		return StatusDraft
	case f.OpenStop != nil:
		return StatusBlocked
	case f.Exported:
		return StatusInReview
	default:
		return StatusRunning
	}
}
