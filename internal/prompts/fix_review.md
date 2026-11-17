A review of your work found these problems:
{{range .Findings}}
{{.Number}}. {{.Severity}}
   Where: {{.Where}}
   What: {{.What}}
   Why: {{.Why}}
{{- end}}

Fix each one, and finish with the structured result.
