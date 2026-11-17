Another agent did this task, and a review of its work found problems it did not fix. You take the task over.

# {{.Title}}

{{.Body}}

# The work so far

The work so far is committed on the current branch. `{{.DiffFile}}` holds it: git's summary, then the diff from the commit the task started from.

# The problems
{{range .Findings}}
{{.Number}}. {{.Severity}}
   Where: {{.Where}}
   What: {{.What}}
   Why: {{.Why}}
{{- end}}

Fix each one, and finish with the structured result.
