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
