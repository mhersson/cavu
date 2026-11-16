package engine

import "strings"

const maxSlug = 50

// Slug lowercases, maps every run of characters outside [a-z0-9] to one "-",
// trims "-", cuts to 50 characters, and returns "task" for an empty result.
// Only [a-z0-9-] survives, so no title can make an invalid ref or smuggle in an
// option or shell syntax.
func Slug(title string) string {
	var b strings.Builder

	inRun := false

	for _, r := range strings.ToLower(title) {
		switch {
		case 'a' <= r && r <= 'z', '0' <= r && r <= '9':
			b.WriteRune(r)

			inRun = false
		case !inRun:
			b.WriteByte('-')

			inRun = true
		}
	}

	s := strings.Trim(b.String(), "-")
	if len(s) > maxSlug {
		s = strings.TrimRight(s[:maxSlug], "-")
	}

	if s == "" {
		return "task"
	}

	return s
}

// BranchName is the task branch for title.
func BranchName(title string) string {
	return "cavu/" + Slug(title)
}
