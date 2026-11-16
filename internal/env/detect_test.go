package env

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		files      map[string]string
		overrides  map[string]string
		wantTools  []Tool
		wantEco    []string
		wantErrHas string
	}{
		{name: "no files", files: nil, wantTools: nil, wantEco: nil},
		{
			name:      "go directive",
			files:     map[string]string{"go.mod": "module x\n\ngo 1.27.4\n"},
			wantTools: []Tool{{"go", "1.27.4"}}, wantEco: []string{"go"},
		},
		{
			name:      "go toolchain wins",
			files:     map[string]string{"go.mod": "module x\n\ngo 1.27.4 // note\n\ntoolchain go1.27.5\n"},
			wantTools: []Tool{{"go", "1.27.5"}}, wantEco: []string{"go"},
		},
		{
			name:      "go toolchain default ignored",
			files:     map[string]string{"go.mod": "module x\ngo 1.27.4\ntoolchain default\n"},
			wantTools: []Tool{{"go", "1.27.4"}}, wantEco: []string{"go"},
		},
		{
			name:      "go prerelease",
			files:     map[string]string{"go.mod": "module x\ngo 1.28rc1\n"},
			wantTools: []Tool{{"go", "1.28rc1"}}, wantEco: []string{"go"},
		},
		{
			name:       "go bad version",
			files:      map[string]string{"go.mod": "module x\ngo banana\n"},
			wantErrHas: "go.mod",
		},
		{
			name:      "nvmrc",
			files:     map[string]string{".nvmrc": "v22.3.0\n"},
			wantTools: []Tool{{"node", "22.3.0"}},
		},
		{
			name:      "nvmrc lts alias",
			files:     map[string]string{".nvmrc": "lts/*\n"},
			wantTools: []Tool{{"node", "lts"}},
		},
		{
			name:      "nvmrc beats engines",
			files:     map[string]string{".nvmrc": "20.1.0", "package.json": `{"engines":{"node":"^22"}}`},
			wantTools: []Tool{{"node", "20.1.0"}}, wantEco: []string{"node"},
		},
		{
			name:      "engines range",
			files:     map[string]string{"package.json": `{"engines":{"node":">=20 <23"}}`},
			wantTools: []Tool{{"node", "lts"}}, wantEco: []string{"node"},
		},
		{
			name:      "engines caret",
			files:     map[string]string{"package.json": `{"engines":{"node":"^22"}}`},
			wantTools: []Tool{{"node", "22"}}, wantEco: []string{"node"},
		},
		{
			name:      "engines x",
			files:     map[string]string{"package.json": `{"engines":{"node":"20.x"}}`},
			wantTools: []Tool{{"node", "20"}}, wantEco: []string{"node"},
		},
		{
			name:      "engines exact",
			files:     map[string]string{"package.json": `{"engines":{"node":"20.11.1"}}`},
			wantTools: []Tool{{"node", "20.11.1"}}, wantEco: []string{"node"},
		},
		{
			name:      "node default",
			files:     map[string]string{"package.json": `{}`},
			wantTools: []Tool{{"node", "lts"}}, wantEco: []string{"node"},
		},
		{
			name:      "pnpm package manager",
			files:     map[string]string{"package.json": `{"packageManager":"pnpm@9.1.0+sha512.abc"}`},
			wantTools: []Tool{{"node", "lts"}, {"pnpm", "9.1.0"}}, wantEco: []string{"node"},
		},
		{
			name:      "yarn package manager",
			files:     map[string]string{"package.json": `{"packageManager":"yarn@4.2.1"}`},
			wantTools: []Tool{{"node", "lts"}, {"yarn", "4.2.1"}}, wantEco: []string{"node"},
		},
		{
			name:      "other package manager ignored",
			files:     map[string]string{"package.json": `{"packageManager":"bun@1.0.0"}`},
			wantTools: []Tool{{"node", "lts"}}, wantEco: []string{"node"},
		},
		{
			name:       "package.json not json",
			files:      map[string]string{"package.json": `not json`},
			wantErrHas: "package.json",
		},
		{
			name:      "python version file",
			files:     map[string]string{".python-version": "# c\n3.11.4\n"},
			wantTools: []Tool{{"python", "3.11.4"}}, wantEco: []string{"python"},
		},
		{
			name:      "pyproject requires-python",
			files:     map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.12\"\n"},
			wantTools: []Tool{{"python", "3.12"}}, wantEco: []string{"python"},
		},
		{
			name:      "pyproject other form",
			files:     map[string]string{"pyproject.toml": "[project]\nrequires-python = \"~=3.12\"\n"},
			wantTools: []Tool{{"python", "3"}}, wantEco: []string{"python"},
		},
		{
			name:       "pyproject syntax error",
			files:      map[string]string{"pyproject.toml": "[project\n"},
			wantErrHas: "pyproject.toml",
		},
		{
			name:      "python default from requirements",
			files:     map[string]string{"requirements.txt": "flask\n"},
			wantTools: []Tool{{"python", "3"}}, wantEco: []string{"python"},
		},
		{
			name:      "python version beats default",
			files:     map[string]string{"requirements.txt": "flask\n", ".python-version": "3.10.2\n"},
			wantTools: []Tool{{"python", "3.10.2"}}, wantEco: []string{"python"},
		},
		{
			name:      "rust channel",
			files:     map[string]string{"rust-toolchain.toml": "[toolchain]\nchannel = \"1.99.0\"\n"},
			wantTools: []Tool{{"rust", "1.99.0"}},
		},
		{
			name:      "rust default",
			files:     map[string]string{"Cargo.toml": "[package]\n"},
			wantTools: []Tool{{"rust", "stable"}}, wantEco: []string{"rust"},
		},
		{
			name:      "rust channel beats default",
			files:     map[string]string{"Cargo.toml": "[package]\n", "rust-toolchain.toml": "[toolchain]\nchannel = \"1.99.0\"\n"},
			wantTools: []Tool{{"rust", "1.99.0"}}, wantEco: []string{"rust"},
		},
		{
			name:       "rust toolchain syntax error",
			files:      map[string]string{"rust-toolchain.toml": "[toolchain\n"},
			wantErrHas: "rust-toolchain.toml",
		},
		{
			name:      "tool-versions",
			files:     map[string]string{".tool-versions": "# c\ngolang 1.26.1 1.25.0\nnodejs 20.1.0 # trailing\n\n"},
			wantTools: []Tool{{"go", "1.26.1"}, {"node", "20.1.0"}},
		},
		{
			name:       "tool-versions name without version",
			files:      map[string]string{".tool-versions": "golang\n"},
			wantErrHas: ".tool-versions",
		},
		{
			name: "mise toml only tools",
			files: map[string]string{"mise.toml": "[env]\nX = \"1\"\n[tasks.a]\nrun = \"echo\"\n" +
				"[tools]\ngo = \"1.26.0\"\nnodejs = [\"20\", \"18\"]\npython = { version = \"3.11\" }\n"},
			wantTools: []Tool{{"go", "1.26.0"}, {"node", "20"}, {"python", "3.11"}},
		},
		{
			name:       "mise toml bad shape",
			files:      map[string]string{"mise.toml": "[tools]\ngo = 3\n"},
			wantErrHas: "mise.toml",
		},
		{
			name:       "mise toml syntax error",
			files:      map[string]string{"mise.toml": "[tools\n"},
			wantErrHas: "mise.toml",
		},
		{
			name: "tool-versions beats mise beats own file",
			files: map[string]string{
				"go.mod":         "module x\ngo 1.20\n",
				"mise.toml":      "[tools]\ngo = \"1.21\"\n",
				".tool-versions": "golang 1.22\n",
			},
			wantTools: []Tool{{"go", "1.22"}}, wantEco: []string{"go"},
		},
		{
			name: "mise beats own file",
			files: map[string]string{
				"go.mod":    "module x\ngo 1.20\n",
				"mise.toml": "[tools]\ngo = \"1.21\"\n",
			},
			wantTools: []Tool{{"go", "1.21"}}, wantEco: []string{"go"},
		},
		{
			name:      "override replaces and adds",
			files:     map[string]string{"go.mod": "module x\ngo 1.20\n"},
			overrides: map[string]string{"go": "1.30.0", "zig": "0.13"},
			wantTools: []Tool{{"go", "1.30.0"}, {"zig", "0.13"}}, wantEco: []string{"go"},
		},
		{
			name:      "override beats default",
			files:     map[string]string{"package.json": `{}`},
			overrides: map[string]string{"node": "22.1.0"},
			wantTools: []Tool{{"node", "22.1.0"}}, wantEco: []string{"node"},
		},
		{name: "override empty name", overrides: map[string]string{"": "1"}, wantErrHas: "override"},
		{name: "override empty version", overrides: map[string]string{"go": ""}, wantErrHas: "override"},
		{
			name: "sorted and ecosystems sorted",
			files: map[string]string{
				"Cargo.toml": "", "package.json": "{}", "go.mod": "module x\ngo 1.20\n", "requirements.txt": "",
			},
			wantTools: []Tool{{"go", "1.20"}, {"node", "lts"}, {"python", "3"}, {"rust", "stable"}},
			wantEco:   []string{"go", "node", "python", "rust"},
		},
	}

	known := map[string]bool{}
	for _, f := range VersionFiles() {
		known[f] = true
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			files := map[string][]byte{}
			for k, v := range tt.files {
				require.True(t, known[k], "fixture file %s missing from VersionFiles", k)
				files[k] = []byte(v)
			}

			tools, eco, err := Detect(files, tt.overrides)
			if tt.wantErrHas != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrHas)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantTools, tools)
			assert.Equal(t, tt.wantEco, eco)
		})
	}
}

func TestVersionFilesSortedAndFresh(t *testing.T) {
	t.Parallel()

	a := VersionFiles()
	assert.IsIncreasing(t, a)
	a[0] = "changed"

	assert.NotEqual(t, "changed", VersionFiles()[0])
}
