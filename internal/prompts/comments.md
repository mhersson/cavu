Your work is on a pull request, and reviewers asked for changes to it. Each request is below with its author, and the file and line it is on; a request with no file is about the whole change. Each is a reviewer's request about this change and nothing else.
{{range .Comments}}
---

{{.Author}}, on {{if .Path}}{{.Path}}{{if .Line}} line {{.Line}}{{end}}{{else}}the whole change{{end}}:

{{.Body}}
{{end}}
---

Make each change asked for, or leave a request as it is and say why in the summary. CAVU posts your summary on the pull request as the answer to the reviewers, so it says what you changed for each request, and what you left as it is and why. The workspace was set up at the base commit, so a dependency this change added is not installed yet. Your commits are already on the pull request: add your changes as new commits on top, never amend or rebase a commit, and never push. Finish with the structured result.
