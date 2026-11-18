A review of your work found these problems:
{{range .Findings}}
{{.Number}}. {{.Severity}}
   Where: {{.Where}}
   What: {{.What}}
   Why: {{.Why}}
{{- end}}

Fix each one, and finish with the structured result. Leave a problem you cannot fix, or judge wrong, as it is, say why in the summary, and finish with status done. The result's title and summary describe the whole change as it now stands, not only this fix, and what you left and why.
