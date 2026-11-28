# The batch

This is task {{.Position}} of {{.Size}} in the batch "{{.Name}}".
{{- if .Before}} These tasks came before it, in this order, and the workspace already holds their commits:
{{range .Before}}
- {{.}}
{{- end}}

The workspace was set up at the base commit, so a dependency an earlier task added is not installed yet.
{{- end -}}
