Do this task.

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
