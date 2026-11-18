package engine_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/cavu/internal/engine"
)

func TestFlag(t *testing.T) {
	var thirty []engine.ChangedFile

	var twenty []string

	for i := range 30 {
		p := fmt.Sprintf(".github/workflows/w%02d.yml", i)
		thirty = append(thirty, engine.ChangedFile{Path: p, Added: true, Size: 10})

		if i < 20 {
			twenty = append(twenty, p)
		}
	}

	for _, tt := range []struct {
		name  string
		files []engine.ChangedFile
		want  engine.Flagged
	}{
		{name: "an ordinary change", files: []engine.ChangedFile{{Path: "src/login.go", Size: 300}, {Path: "README.md", Added: true, Size: 40}}},
		{name: "a workflow file", files: []engine.ChangedFile{{Path: ".github/workflows/ci.yml", Size: 500}}, want: engine.Flagged{Workflows: []string{".github/workflows/ci.yml"}}},
		{name: "a deleted workflow file", files: []engine.ChangedFile{{Path: ".github/workflows/ci.yml"}}, want: engine.Flagged{Workflows: []string{".github/workflows/ci.yml"}}},
		{
			name:  "instruction files at the root and nested",
			files: []engine.ChangedFile{{Path: "AGENTS.md", Size: 9}, {Path: "web/CLAUDE.md", Added: true, Size: 9}, {Path: "docs/agents.md", Added: true, Size: 9}},
			want:  engine.Flagged{Instructions: []string{"AGENTS.md", "web/CLAUDE.md"}},
		},
		{name: "a deleted AGENTS.md", files: []engine.ChangedFile{{Path: "AGENTS.md"}}, want: engine.Flagged{Instructions: []string{"AGENTS.md"}}},
		{name: "a path under .claude/", files: []engine.ChangedFile{{Path: ".claude/settings.json", Size: 80}}, want: engine.Flagged{Instructions: []string{".claude/settings.json"}}},
		{
			name:  "a path under a nested .claude/",
			files: []engine.ChangedFile{{Path: "pkg/.claude/settings.json", Size: 80}, {Path: "pkg/x.claude/settings.json", Size: 80}},
			want:  engine.Flagged{Instructions: []string{"pkg/.claude/settings.json"}},
		},
		{name: "an added file of 2 MiB", files: []engine.ChangedFile{{Path: "assets/video.mp4", Added: true, Size: 2 << 20}}, want: engine.Flagged{Large: []string{"assets/video.mp4"}}},
		{name: "an added file of 1 MiB", files: []engine.ChangedFile{{Path: "assets/logo.png", Added: true, Size: 1 << 20}}},
		{name: "a modified file of 2 MiB", files: []engine.ChangedFile{{Path: "assets/video.mp4", Size: 2 << 20}}},
		{name: "30 workflow files", files: thirty, want: engine.Flagged{Workflows: twenty}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, engine.Flag(tt.files))
		})
	}
}
