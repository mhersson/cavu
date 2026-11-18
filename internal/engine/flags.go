package engine

import (
	"path"
	"strings"
)

const (
	// maxFlagged bounds each list of a flagged change.
	maxFlagged = 20
	// largeFile is the size over which a file a change adds is flagged.
	largeFile = 1 << 20
)

// Flagged names the paths of a change that a person reading it should know
// of, at most maxFlagged of each kind.
type Flagged struct {
	Workflows    []string `json:"workflows"`    // under .github/workflows/
	Instructions []string `json:"instructions"` // AGENTS.md or CLAUDE.md in any directory, or under .claude/
	Large        []string `json:"large"`        // added, and over 1 MiB
}

// Flag flags workflows and instruction files on any path files list, a
// deleted one included, and large files only where the change adds them.
func Flag(files []ChangedFile) Flagged {
	var f Flagged

	add := func(list *[]string, p string) {
		if len(*list) < maxFlagged {
			*list = append(*list, p)
		}
	}

	for _, c := range files {
		if strings.HasPrefix(c.Path, ".github/workflows/") {
			add(&f.Workflows, c.Path)
		}

		if name := path.Base(c.Path); name == "AGENTS.md" || name == "CLAUDE.md" || strings.HasPrefix(c.Path, ".claude/") {
			add(&f.Instructions, c.Path)
		}

		if c.Added && c.Size > largeFile {
			add(&f.Large, c.Path)
		}
	}

	return f
}
