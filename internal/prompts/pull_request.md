CAVU wrote this pull request for task {{.TaskID}}: an agent made the change unattended, and no person has read it yet.
{{- with .Flagged}}
{{- if or .Workflows .Instructions .Large}}

Read these first:
{{range .Workflows}}
- `{{.}}` is a CI workflow.
{{- end}}
{{- range .Instructions}}
- `{{.}}` holds instructions to coding agents.
{{- end}}
{{- range .Large}}
- `{{.}}` is a file over 1 MiB that the change adds.
{{- end}}
{{- end}}
{{- end}}

## What

{{with .Summary}}{{.}}{{else}}The agent gave no summary of its change.{{end}}

## Why

The task as a person wrote it:

**{{.TaskTitle}}**
{{- with .TaskBody}}

{{.}}
{{- end}}

## Checks

{{if .VerifyWaived -}}
- Verify did not run: the project waives its verify command, so nothing ran the project's checks on this change.
{{- else -}}
- Verify passed: CAVU ran `{{.VerifyCommand}}` on this change.
{{- end}}
{{- if .ReadinessOff}}
- No readiness turn ran: the agent did not first check the task for facts it lacked, since the project turns that off.
{{- end}}
{{- if .ReviewOff}}
- No review ran: the project turns the review off.
{{- else if .ReviewFailed}}
- The review gave no verdict: {{.ReviewFailed}}
{{- end}}
{{- with .WhereToLook}}

## Where to look
{{range .}}
- {{.}}
{{- end}}
{{- end}}
{{- with .Assumptions}}

## Assumptions

The agent made these choices without asking:
{{range .}}
- {{.}}
{{- end}}
{{- end}}
{{- if or .Open .Minor}}

## Review findings left open

The review found these, and they are still open:
{{range .Open}}
{{template "pull_request_finding" .}}
{{- end}}
{{- range .Minor}}
{{template "pull_request_finding" .}}
{{- end}}
{{- end}}

---

Model `{{.Model}}`{{with .Effort}} at effort `{{.}}`{{end}}. The task took {{.Duration}} from its start to this change.
{{- define "pull_request_finding"}}- **{{.Severity}}**{{with .Where}} `{{.}}`{{end}}: {{.What}}{{with .Why}} {{.}}{{end}}{{end}}
