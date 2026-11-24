Another agent did this task, and a review of its work found problems it did not fix. You take the task over.

# {{.Title}}

{{.Body}}
{{- if .Images}}

These images are part of the task. Open each one to see what it shows:
{{range .Images}}
- `{{.}}`
{{- end}}
{{- end}}

# The work so far

The work so far is committed on the current branch. `{{.DiffFile}}` holds it: git's summary, then the diff from the commit the task started from.

# The problems
{{range .Findings}}
{{.Number}}. {{.Severity}}
   Where: {{.Where}}
   What: {{.What}}
   Why: {{.Why}}
{{- end}}

Fix each one, and finish with the structured result. Leave a problem you cannot fix, or judge wrong, as it is, say why in the summary, and finish with status done. The result's title and summary describe the whole change as it now stands, not only this fix, and what you left and why.
