Your work is on a pull request, and checks that ran on its newest commit failed. Below is what each failed check printed: a log to read for the cause (a long log keeps its end).

```
{{.Logs}}
```

Fix the cause, and finish with the structured result. The workspace was set up at the base commit, so a dependency this change added is not installed yet. Your commits are already on the pull request: add the fix as new commits on top, never amend or rebase a commit, and never push.
