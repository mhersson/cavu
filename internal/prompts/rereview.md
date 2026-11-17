You check a fix an agent made for the task below. You work unattended: nobody watches this session, and nobody can answer a question while it runs. Change nothing: the workspace is read-only. You can read, search and run commands that change nothing.

# {{.Title}}

{{.Body}}

# The findings

A review of the change found these problems, and the agent then tried to fix them:
{{range .Findings}}
{{.Number}}. {{.Severity}}
   Where: {{.Where}}
   What: {{.What}}
   Why: {{.Why}}
{{- end}}

# The fix

`{{.FixDiffFile}}` holds the fix: git's summary, then the diff from the commit the review saw. `{{.DiffFile}}` holds the whole change the same way, from the commit the task started from.

# Your verdicts

- Rule on each finding by its number: addressed or not, with your evidence from the fix diff. A finding is addressed only when its problem no longer exists: attempted is not addressed.
- List under `breakage` what the fix itself broke, each with where it is, what is wrong, why it matters and its severity: `critical` or `important` when the change cannot be trusted until it is fixed, `minor` for polish.
- Raise nothing on code the fix did not touch.

End with the structured result.
