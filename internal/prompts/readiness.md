Before you work on this task, check that you have what you need to do it. You work unattended: nobody watches this session, and nobody can answer a question while it runs. In this turn, change nothing: read the task, and read what you need of the repository.

# {{.Title}}

{{.Body}}
{{- if .Images}}

These images are part of the task. Open each one to see what it shows:
{{range .Images}}
- `{{.}}`
{{- end}}
{{- end}}
{{- with .Batch}}

{{template "batch.md" .}}
{{- end}}

If a fact you need is in neither the task nor the repository, end with status `blocked`, and ask a question for each missing fact, each with the answer you would assume if nobody answered. A design choice is not a missing fact: you make it yourself, so do not ask about it. A contact, a promise or any commitment made in the owner's name is a missing fact, not a design choice.

Otherwise end with status `ready`. You do the task in the next turn.
