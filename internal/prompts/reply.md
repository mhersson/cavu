CAVU wrote this comment for task {{.TaskID}}, after an agent worked unattended on the reviewers' requests on this pull request.

{{with .Summary}}{{.}}{{else}}The agent gave no summary of its work.{{end}}

{{if .Changed -}}
The agent changed the code: its new commits are on this pull request.{{if .Summary}} Its summary above says what it left as it is, and why.{{end}}
{{- else -}}
The agent left the code as it is: it added no commit to this pull request.{{if .Summary}} Its summary above says why.{{end}}
{{- end}}
